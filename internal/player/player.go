package player

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/audio"

	"mygo-music/internal/i18n"
	"mygo-music/internal/lyrics"
	"mygo-music/internal/music"
)

type RepeatMode int

const (
	RepeatOff RepeatMode = iota
	RepeatAll
	RepeatOne
)

// Player plays a queue of tracks, from whichever source each is of. Its
// methods run on the main thread; loading happens on goroutines, whose results
// post brings back there.
type Player struct {
	post func(func())
	// Quiet is post without a redraw, for what mostly changes nothing, if set.
	Quiet func(func())
	// Source finds the source of an ID, or nil if it's gone.
	Source func(id string) music.Source
	// Failed, if set, hears that a source said the listener has something to
	// do first: ErrSignIn or ErrSetup.
	Failed func(source string, err error)
	// Quality returns the quality to ask sources for, as the settings say.
	Quality func() string
	// Normalize reports whether to even out the loudness of tracks, as
	// the settings say.
	Normalize func() bool
	// Dolby returns the current settings for the AAF Dolby Audio processor.
	Dolby func() DolbyProfile
	// Automix enables automatic crossfades between queued tracks.
	Automix func() bool
	// Demucs selects the AI spatial upmix device for ordinary tracks.
	Demucs func() DemucsMode
	// DemucsClarity enables high-frequency compensation on the rendered Demucs bed.
	DemucsClarity func() bool
	// DemucsMix adjusts the separated stems before rendering.
	DemucsMix func() DemucsMix
	// DemucsPositioning enables adaptive stem placement in the AAF bed.
	DemucsPositioning func() bool
	// Cache keeps the songs played, if set.
	Cache *Cache
	// Changed, if set, hears that what plays, where it is or whether it
	// plays changed.
	Changed func()

	queue   []music.Track
	order   []int // the queue's indices in play order: shuffled or not
	at      int   // where in order the current track is, -1 for none
	shuffle bool
	repeat  RepeatMode
	volume  float64

	// A radio goes on with the tracks its feed gives, as the queue nears
	// its end, for as long as nothing else is played.
	radio    string
	feed     func(got func([]music.Track))
	radioGen int  // counts radios, to drop what an old one gives
	feeding  bool // the feed is asked
	awaiting bool // at the queue's end, for what the feed gives

	track             *music.Track
	out               *audio.Player
	handoff           *audio.Player
	automixPrep       *automixPreparation
	automixMix        *automixPlan
	nextRhythm        *nextRhythm
	metronome         bool
	metronomeVolume   float64
	stream            stream
	watch             *watched // stream, as out plays it
	dolby             DolbyProcessing
	demucsUnavailable bool
	info              *music.Stream // what the current track streams
	fade              fader
	gain              float64 // the current track's normalizing gain
	loading           bool
	playing           bool // what the listener wants: false while paused
	err               string
	gen               int // counts loads, to drop the results of stale ones
	cancelOpen        context.CancelFunc

	lyrics       []lyrics.Line
	lyricsLoaded bool

	// pending holds a seek asked for while the track loads.
	pending time.Duration
	// A seek playing fades out, then lands, then fades in: meanwhile the
	// position is where it goes.
	seeking bool
	seekTo  time.Duration
	seekGen int

	// remote is set while the current track is a PlaybackRemote source's,
	// which plays it elsewhere: see remote.go.
	remote *remote
	// after runs fn after d, on another goroutine; tests replace it.
	after func(d time.Duration, fn func())
	now   func() time.Time
	cmds  commands
	// The volume last asked of a remote source, and whether that's on its
	// way: guarded by cmds.mu.
	volumeAsked  int
	volumeQueued bool

	recorded string // the track last told its source it played

	ahead         *ahead // the next track's audio, asked for before its turn
	demucs        *demucsEngine
	demucsContext context.Context
	cancelDemucs  context.CancelFunc
}

func New(post func(func())) *Player {
	p := &Player{post: post, at: -1, volume: 0.8, gain: 1, metronomeVolume: 1}
	p.after = func(d time.Duration, fn func()) { time.AfterFunc(d, fn) }
	p.now = time.Now
	// Music, mixed far enough ahead to play on while macOS keeps the app
	// waiting, as it does while its appearance changes.
	audio.SetLatency(audio.LatencyPlayback)
	// The media keys, and the controls of Control Center.
	mygo.NowPlaying.OnCommand(func(cmd mygo.MediaCommand, pos time.Duration) {
		switch cmd {
		case mygo.MediaPlay, mygo.MediaPause:
			if (cmd == mygo.MediaPlay) != p.playing {
				p.TogglePlay()
			}
		case mygo.MediaNext:
			p.Next()
		case mygo.MediaPrevious:
			p.Prev()
		case mygo.MediaSeek:
			p.Seek(pos)
		}
		p.post(func() {})
	})
	return p
}

// sync shows the system, and Changed, where playback is.
func (p *Player) sync() {
	if p.Changed != nil {
		defer p.Changed()
	}
	if p.track == nil {
		mygo.NowPlaying.Clear()
		return
	}
	state := mygo.PlaybackPaused
	if p.playing {
		state = mygo.PlaybackPlaying
	}
	mygo.NowPlaying.SetPlayback(state, p.Position())
}

// announce shows the system the track, and its cover once loaded.
func (p *Player) announce(t *music.Track, artwork []byte) {
	state := mygo.PlaybackPaused
	if p.playing {
		state = mygo.PlaybackPlaying
	}
	mygo.NowPlaying.Set(mygo.NowPlayingInfo{
		Title:    t.Name,
		Artist:   t.ArtistNames(),
		Album:    t.Album.Name,
		Artwork:  artwork,
		Duration: t.Length(),
		Position: p.Position(),
		State:    state,
		Commands: mygo.MediaPlay | mygo.MediaPause | mygo.MediaNext | mygo.MediaPrevious | mygo.MediaSeek,
	})
}

func (p *Player) ended() {
	if p.repeat == RepeatOne {
		p.out.Seek(0)
		p.out.Play()
		return
	}
	if p.at+1 >= len(p.order) && p.feed != nil {
		p.awaitFeed()
		return
	}
	if p.at+1 >= len(p.order) && p.repeat == RepeatOff {
		p.playing = false
		p.sync()
		return
	}
	p.Next()
}

// PlayQueue replaces the queue with tracks and plays the one at start, or the
// first after it that can play.
func (p *Player) PlayQueue(tracks []music.Track, start int) {
	p.playQueue(tracks, start, forward)
}

// PlayTrack replaces the queue with tracks and plays the one at i, the
// listener's pick: one that can't play stays, saying why, rather than another
// playing in its place.
func (p *Player) PlayTrack(tracks []music.Track, i int) {
	p.playQueue(tracks, i, stay)
}

// PlayShuffled replaces the queue with tracks in a random order, any of them
// first, and plays it with shuffle on.
func (p *Player) PlayShuffled(tracks []music.Track) {
	p.shuffle = true
	p.playQueue(tracks, -1, forward)
}

// playQueue plays tracks from start; a start below 0, when shuffled, lets
// any track come first.
func (p *Player) playQueue(tracks []music.Track, start, dir int) {
	if len(tracks) == 0 {
		return
	}
	p.endRadio()
	start = min(start, len(tracks)-1)
	p.queue = append([]music.Track(nil), tracks...)
	p.order = make([]int, len(p.queue))
	for i := range p.order {
		p.order[i] = i
	}
	if p.shuffle {
		p.shuffleOrder(start)
		start = 0
	}
	p.loadSkipping(max(start, 0), 0, dir, true, 0)
}

// PlayRadio plays tracks in their order, then goes on with those feed
// gives as the queue nears its end, until something else is played. The
// radio's name says which it is. feed calls got on the main thread.
func (p *Player) PlayRadio(name string, tracks []music.Track, feed func(got func([]music.Track))) {
	if len(tracks) == 0 {
		return
	}
	shuffle := p.shuffle
	p.shuffle = false
	p.PlayQueue(tracks, 0)
	p.shuffle = shuffle
	p.AttachRadio(name, feed)
}

// AttachRadio makes the queue the radio name's, which feed goes on with,
// as after restoring a session that played it.
func (p *Player) AttachRadio(name string, feed func(got func([]music.Track))) {
	p.endRadio()
	p.radio, p.feed = name, feed
	p.refill()
}

// EndRadio stops the radio playing: the song playing plays on, but the
// songs the radio queued after it go, and it adds no more.
func (p *Player) EndRadio() {
	if p.radio == "" && p.feed == nil {
		return
	}
	awaiting := p.awaiting
	p.endRadio()
	if p.at >= 0 && p.at+1 < len(p.order) {
		kept := make([]music.Track, p.at+1)
		for i, k := range p.order[:p.at+1] {
			kept[i] = p.queue[k]
		}
		p.queue, p.order = kept, make([]int, len(kept))
		for i := range p.order {
			p.order[i] = i
		}
	}
	p.refreshAhead()
	if awaiting {
		// At its end, waiting for more: nothing plays on.
		p.playing = false
		p.sync()
	}
}

func (p *Player) endRadio() {
	p.radioGen++
	p.radio, p.feed, p.feeding, p.awaiting = "", nil, false, false
}

// Radio returns the name of the radio playing, or "".
func (p *Player) Radio() string { return p.radio }

// refill asks the radio for more once the queue is near its end.
func (p *Player) refill() {
	if p.feed == nil || p.feeding || p.at < len(p.order)-2 {
		return
	}
	p.feeding = true
	gen := p.radioGen
	p.feed(func(more []music.Track) {
		if gen != p.radioGen {
			return
		}
		p.feeding = false
		p.appendTracks(more)
		switch {
		case !p.awaiting:
		case p.at+1 < len(p.order):
			p.awaiting = false
			p.load(p.at + 1)
		default:
			// The radio has nothing more: it stops at its end.
			p.awaiting, p.playing = false, false
			p.err = i18n.T("The station has nothing more right now")
			p.sync()
		}
	})
}

// awaitFeed waits at the end of a radio's queue for what its feed gives.
func (p *Player) awaitFeed() {
	p.awaiting = true
	p.refill()
}

// appendTracks adds tracks to the end of the queue, but those in it.
func (p *Player) appendTracks(tracks []music.Track) {
	type key struct{ source, id string }
	in := make(map[key]bool, len(p.queue))
	for _, t := range p.queue {
		in[key{t.Source, t.ID}] = true
	}
	for _, t := range tracks {
		if k := (key{t.Source, t.ID}); !in[k] {
			in[k] = true
			p.order = append(p.order, len(p.queue))
			p.queue = append(p.queue, t)
		}
	}
	p.refreshAhead()
}

// shuffleOrder puts the order in a random one with first at its head, or
// with any at its head when first is below 0.
func (p *Player) shuffleOrder(first int) {
	rand.Shuffle(len(p.order), func(i, j int) { p.order[i], p.order[j] = p.order[j], p.order[i] })
	for i, v := range p.order {
		if v == first {
			p.order[0], p.order[i] = p.order[i], p.order[0]
		}
	}
}

func (p *Player) ToggleShuffle() {
	p.shuffle = !p.shuffle
	if len(p.order) == 0 {
		return
	}
	cur := p.order[max(p.at, 0)]
	if p.shuffle {
		p.shuffleOrder(cur)
		p.at = 0
	} else {
		for i := range p.order {
			p.order[i] = i
		}
		p.at = cur
	}
	p.refreshAhead()
}

func (p *Player) CycleRepeat() {
	p.repeat = (p.repeat + 1) % 3
	p.refreshAhead()
}

// Current returns the track playing or loading, or nil.
func (p *Player) Current() *music.Track { return p.track }

// Queue returns the tracks queued, in the order they were given.
func (p *Player) Queue() []music.Track { return p.queue }

// Playing reports what the listener wants: false while paused.
func (p *Player) Playing() bool { return p.playing }

// DolbyStatus reports whether this track has started Dolby Audio processing.
func (p *Player) DolbyStatus() (active, tried bool) {
	if p.dolby.Pending {
		return false, false
	}
	return p.dolby.Active, p.dolby.Profile.enabled()
}

// DolbyProcessing returns the last decoder status on the main thread.
func (p *Player) DolbyProcessing() DolbyProcessing {
	return p.dolby
}

// DolbyAtmos reports whether the track has a decoded Atmos object presentation.
func (p *Player) DolbyAtmos() bool {
	s, ok := p.stream.(*codecPCM)
	return ok && s.format.channels == atmosPCMChannels
}

// DemucsStatus describes the current track's asynchronous stem processing.
func (p *Player) DemucsStatus() DemucsStatus {
	s, ok := p.stream.(*demucsStream)
	if !ok {
		if p.demucs != nil && (p.track == nil || p.loading) {
			return p.demucs.status()
		}
		return DemucsStatus{Requested: p.demucsUnavailable, Failed: p.demucsUnavailable}
	}
	state := s.engine.status()
	if stats := s.processing.Load(); stats != nil {
		state.Active, state.Device, state.Speed = true, stats.device, stats.speed
	}
	state.Failed = state.Failed || s.failed.Load()
	return state
}

// ApplyDolby queues a profile switch on the decoder goroutine.
func (p *Player) ApplyDolby() {
	if p.watch == nil || p.Dolby == nil {
		return
	}
	d, ok := p.watch.stream.(*dolbyStream)
	if !ok {
		return
	}
	d.clarityOn.Store(p.DemucsClarity != nil && p.DemucsClarity())
	p.applyDemucsMix()
	profile := p.Dolby()
	if *d.requested.Load() == profile {
		return
	}
	d.requested.Store(&profile)
	p.dolby.Pending = true
}

// Loading reports whether the current track is still loading.
// Loading reports whether the current track is on its way: loading, or
// waiting for the next of its bytes, after a seek past what has come in.
func (p *Player) Loading() bool {
	return p.loading || p.awaiting || p.watch != nil && p.watch.starved()
}

// Ready reports whether the current track has loaded, to play.
func (p *Player) Ready() bool { return p.out != nil || p.remote != nil && p.remote.started }

// Err returns why the current track did not play, or "".
func (p *Player) Err() string { return p.err }

// Stream returns what the current track streams, once loaded, or nil.
func (p *Player) Stream() *music.Stream { return p.info }

func (p *Player) Shuffle() bool      { return p.shuffle }
func (p *Player) Repeat() RepeatMode { return p.repeat }

// Lyrics returns the current track's lyrics, and whether they have loaded.
func (p *Player) Lyrics() ([]lyrics.Line, bool) { return p.lyrics, p.lyricsLoaded }

// Downloaded returns the share of the current track downloaded so far, or
// false before it has started to.
func (p *Player) Downloaded() (float64, bool) {
	if p.stream == nil {
		return 0, false
	}
	dl, _ := p.stream.source()
	return dl.Progress(), true
}

// PlayOrder returns the queue's indices in play order, and where the
// current track is among them, -1 for none.
func (p *Player) PlayOrder() ([]int, int) { return p.order, p.at }

func (p *Player) Next() {
	if len(p.order) == 0 {
		return
	}
	at := p.at + 1
	if at >= len(p.order) {
		if p.feed != nil {
			p.stopForFeed()
			p.awaitFeed()
			return
		}
		at = 0
	}
	p.load(at)
}

// Prev plays the track before this one, however far this one has played:
// once a few seconds in, going back to its start only kept the listener
// pressing twice.
func (p *Player) Prev() {
	if len(p.order) == 0 {
		return
	}
	at := p.at - 1
	if at < 0 {
		at = len(p.order) - 1
	}
	// One that can't play is passed going back, as going forward.
	p.loadSkipping(at, 0, back, true, 0)
}

// Jump plays the queue's track i, the listener's pick: one that can't play
// stays, saying why.
func (p *Player) Jump(i int) {
	for k, v := range p.order {
		if v == i {
			p.loadSkipping(k, 0, stay, true, 0)
			return
		}
	}
}

// TogglePlay pauses, fading out, or plays, fading in.
func (p *Player) TogglePlay() {
	if p.remote != nil && p.remote.started {
		p.remoteToggle()
		return
	}
	p.cancelAutomixPrep()
	p.finishHandoff(p.handoff)
	if p.out == nil {
		switch {
		case p.loading:
			// It plays once loaded, or not.
			p.playing = !p.playing
			p.sync()
		case len(p.order) > 0:
			// Again, as for a track that couldn't play.
			p.loadSkipping(max(p.at, 0), 0, stay, true, p.pending)
		}
		return
	}
	out := p.out
	if p.playing {
		p.playing = false
		p.fade.fade(0, fadeOut, func() {
			p.post(func() {
				if p.out == out && !p.playing {
					out.Pause()
					p.sync()
				}
			})
		})
		p.sync()
		return
	}
	p.playing = true
	p.record()
	if !p.seeking && !out.Playing() {
		if out.Position() >= out.Duration()-time.Second {
			out.Seek(0)
		}
		out.Play()
	}
	p.fade.fade(1, fadeIn, nil)
	p.sync()
}

// seekGap is how long a seek waits for what plays to fade out, which the
// audio player does over 5 ms, as it pauses: within the next buffer or
// two it sends the system.
const seekGap = 30 * time.Millisecond

// Seek moves to t. Playing, it pauses and plays again around the jump,
// which the audio player fades over a few milliseconds, rather than
// cutting from one sound to the other with a click.
func (p *Player) Seek(t time.Duration) {
	if p.remote != nil && p.remote.started {
		p.remoteSeek(t)
		return
	}
	p.cancelAutomixPrep()
	p.finishHandoff(p.handoff)
	if p.out == nil {
		p.pending = t
		return
	}
	out := p.out
	// One past what has come in waits for it: a frame once that would show.
	p.after(seekGap+starvedAfter+20*time.Millisecond, func() { p.post(func() {}) })
	// Paused, it jumps at once; but a seek on its way paused it only for
	// the jump, and this one takes its place, playing on from there.
	if !out.Playing() && !p.seeking {
		out.Seek(t)
		p.sync()
		return
	}
	p.seekGen++
	gen := p.seekGen
	p.seeking, p.seekTo = true, t
	out.Pause()
	time.AfterFunc(seekGap, func() {
		p.post(func() {
			if gen != p.seekGen || p.out != out {
				return
			}
			p.seeking = false
			out.Seek(t)
			if p.playing || !p.fade.silent() {
				out.Play()
			}
			p.sync()
		})
	})
	p.sync()
}

func (p *Player) SetVolume(v float64) {
	p.finishHandoff(p.handoff)
	p.volume = v
	p.applyGain()
	if p.remote != nil {
		p.remoteVolume(v)
	}
}

// applyGain sets the volume of what plays: the listener's, which the ear
// hears in powers, and the track's gain, if normalizing.
func (p *Player) applyGain() {
	g := p.volume * p.volume
	if p.Normalize != nil && p.Normalize() {
		g *= p.gain
	}
	p.fade.setGain(g)
}

// normPreamp lifts normalized tracks, which their gains bring to the quiet
// loudness of ReplayGain, nearer to how loud music plays elsewhere.
const normPreamp = 4

// normGain is the gain that brings a track as loud as others, as its source
// measured it, but never so loud that its peaks would clip.
func normGain(st *music.Stream) float64 {
	if st == nil || st.Gain == 0 && st.Peak == 0 {
		return 1
	}
	g := math.Pow(10, (st.Gain+normPreamp)/20)
	if st.Peak > 0 {
		g = min(g, 1/st.Peak)
	}
	return g
}

func (p *Player) Position() time.Duration {
	switch {
	case p.remote != nil && p.remote.started:
		return p.remotePosition()
	case p.out == nil:
		return p.pending
	case p.seeking:
		return p.seekTo
	}
	return p.watch.sourceTime(p.out.Position())
}

func (p *Player) Duration() time.Duration {
	switch {
	case p.track == nil:
		return 0
	case p.remote != nil && p.remote.duration > 0:
		return p.remote.duration
	case p.track.Duration > 0 || p.out == nil:
		return p.track.Length()
	}
	return p.out.Duration() // a source that didn't say
}

// Levels returns the spectrum of what plays now.
func (p *Player) Levels() ([Bands]float32, bool) {
	if p.stream == nil || p.out == nil || !p.playing {
		return [Bands]float32{}, false
	}
	_, spec := p.stream.source()
	frame := int64(p.Position().Seconds() * float64(p.stream.SampleRate()))
	return spec.at(frame)
}

func (p *Player) stop() {
	p.cancelAutomixPrep()
	p.clearNextRhythm()
	p.finishHandoff(p.handoff)
	if p.cancelOpen != nil {
		p.cancelOpen()
		p.cancelOpen = nil
	}
	if s, ok := p.stream.(*demucsStream); ok {
		s.cancel()
	}
	if out := p.out; out != nil {
		retireOutput(out)
	}
	p.out, p.stream, p.watch, p.info, p.remote = nil, nil, nil, nil, nil
	p.dolby = DolbyProcessing{}
	p.demucsUnavailable = false
	p.seeking = false
	p.fade.attach(nil, 1)
}

func retireOutput(out *audio.Player) {
	// Pausing uses the mixer ramp before Close removes the voice.
	out.Pause()
	time.AfterFunc(100*time.Millisecond, out.Close)
}

func (p *Player) finishHandoff(out *audio.Player) {
	if out != nil && p.handoff == out {
		p.handoff = nil
		p.automixMix = nil
		p.fade.setHandoff(nil, nil)
		if p.watch != nil {
			p.watch.setAutomix(nil, true)
		}
		retireOutput(out)
	}
}

const trackHandoffFade = 60 * time.Millisecond

// Which way a load goes past a track that can't play: on to the next, back to
// the one before, or nowhere, staying on it to say why.
const (
	forward = 1
	back    = -1
	stay    = 0
)

// load plays the track at order[at], skipping those that cannot play.
func (p *Player) load(at int) {
	p.loadSkipping(at, 0, forward, true, 0)
}

// stopForFeed stops what plays, at the end of a radio's queue, while its feed
// gives more.
func (p *Player) stopForFeed() {
	if was := p.remote; was != nil && was.started && p.playing {
		p.command(was.src, was.src.RemotePause)
	}
	p.stop()
}

// sourceOf returns the source of track t, or nil if it's gone.
func (p *Player) sourceOf(t *music.Track) music.Source {
	if p.Source == nil {
		return nil
	}
	return p.Source(t.Source)
}

// record tells the source of the current track, if it wants to know, that it
// plays, once for each time it's played.
func (p *Player) record() {
	if p.track == nil || !p.playing {
		return
	}
	src, ok := p.sourceOf(p.track).(music.Recorder)
	key := p.track.Source + "\x00" + p.track.ID
	if !ok || key == p.recorded {
		return
	}
	p.recorded = key
	t := *p.track
	go src.Played(t)
}

// loadSkipping loads the track at order[at], to play or not, from from,
// skipping those that cannot play the way dir goes: skipped of them so far.
func (p *Player) loadSkipping(at, skipped, dir int, play bool, from time.Duration) {
	p.loadTrack(at, skipped, dir, play, from, trackHandoffFade, nil)
}

func (p *Player) loadTrack(at, skipped, dir int, play bool, from, overlap time.Duration, mix *automixPlan) {
	p.cancelAutomixPrep()
	t := p.queue[p.order[at]]
	src := p.sourceOf(&t)
	if was := p.remote; was != nil && was.started && p.playing && (src == nil || src.Info().ID != was.src.Info().ID) {
		// Whatever plays next isn't its to play.
		p.command(was.src, was.src.RemotePause)
	}
	var outgoing *audio.Player
	var outgoingWatch *watched
	if _, demucs := p.stream.(*demucsStream); (mix != nil || demucs && from == 0) && play && p.playing && !p.seeking &&
		p.out != nil && p.out.Playing() && src != nil && src.Info().Playback != music.PlaybackRemote {
		outgoing, p.out = p.out, nil
		outgoingWatch = p.watch
		if p.watch != nil {
			p.watch.clickGain.Store(0)
		}
		p.stream = nil // the outgoing audio player owns its decoder until the handoff ends
	}
	p.stop()
	if outgoing != nil {
		p.handoff = outgoing
		if mix != nil && !mix.timed() {
			mix = &automixPlan{target: mix.target, overlap: overlap}
		}
		p.automixMix = mix
		// Limit how long the previous song carries the transition for a slow source.
		p.after(max(750*time.Millisecond, overlap+automixLead), func() {
			p.post(func() {
				if p.out == nil || !p.out.Playing() {
					p.finishHandoff(outgoing)
				}
			})
		})
	}
	p.gen++
	gen := p.gen
	p.at = at
	p.track = &t
	p.loading, p.playing, p.err = true, play, ""
	p.awaiting = false
	p.lyrics, p.lyricsLoaded = nil, false
	p.pending = from
	p.recorded = ""
	p.announce(&t, nil)
	p.refill()

	p.loadCover(gen, &t)
	p.loadLyrics(gen, src, t)
	if src != nil && src.Info().Playback == music.PlaybackRemote {
		p.loadRemote(gen, src, at, skipped, dir, play, from)
		return
	}
	quality := p.playbackQuality()
	pre := p.takeAhead(t, quality)
	var engine *demucsEngine
	if t.ShowID == "" {
		p.PrepareDemucs()
		engine = p.demucs
		if engine != nil {
			engine.retain()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelOpen = cancel
	go func() {
		available := engine != nil && findDemucs() != ""
		info, s, err := p.open(ctx, src, t, quality, pre)
		demucsUnavailable := false
		if err == nil && engine != nil {
			if _, native := s.(*codecPCM); !native {
				rate := s.SampleRate()
				if available && s.Channels() == 2 && rate >= 32000 && rate <= 192000 {
					s = newDemucsStream(s, engine, t.Duration)
					engine = nil // the producer owns this reference
				} else {
					demucsUnavailable = true
				}
			}
		}
		if engine != nil {
			engine.release()
		}
		cancel()
		p.post(func() {
			if gen != p.gen {
				if s != nil {
					s.Close()
				}
				return
			}
			p.cancelOpen = nil
			p.loading = false
			if err != nil {
				p.finishHandoff(outgoing)
				p.loadFailed(t.Source, err, at, skipped, dir)
				return
			}
			p.stream, p.info = s, info
			p.applyDemucsMix()
			p.demucsUnavailable = demucsUnavailable
			p.gain = normGain(info)
			var decoded stream = s
			if s.Channels() == 2 && p.Dolby != nil {
				d := &dolbyStream{stream: s}
				d.clarityOn.Store(p.DemucsClarity != nil && p.DemucsClarity())
				profile := p.Dolby()
				rate := s.SampleRate()
				p.dolby.Pending = profile.enabled()
				d.requested.Store(&profile)
				d.status = func(used *DolbyProfile, active bool, channels int) {
					p.post(func() {
						if gen == p.gen && p.out != nil && d.requested.Load() == used {
							p.dolby = DolbyProcessing{Profile: *used, SampleRate: rate, InputChannels: channels, Active: active}
						}
					})
				}
				decoded = d
			}
			p.watch = &watched{stream: decoded, now: p.now, ended: func() { p.post(func() {}) }}
			p.watch.rhythm, _ = s.(*demucsStream)
			p.applyMetronome()
			waitForFirstRead := outgoing != nil && p.handoff == outgoing
			if waitForFirstRead {
				if mix.timed() && s.Channels() == 2 && p.pending == mix.cue {
					p.watch.setAutomix(mix, true)
				}
				if p.pending > 0 {
					p.watch.firstFrame = int64(math.Round(p.pending.Seconds() * float64(s.SampleRate())))
				}
				var begin func(bool)
				begin = func(buffered bool) {
					if gen != p.gen || p.out == nil || !p.playing || p.out.Playing() || p.seeking {
						return
					}
					if mix.timed() {
						wait := mix.tempo.mapTime(mix.target, true) - automixMixPosition(outgoing)
						atCue := (p.out.Position() - mix.cue).Abs() < time.Millisecond
						ready := buffered && p.handoff == outgoing && outgoing.Playing() && p.Automix() && s.Channels() == 2 &&
							atCue && (p.Dolby == nil || !p.Dolby().Spatial)
						late := 110 * time.Millisecond
						if mix.native != nil {
							ready = ready && p.watch.rhythm != nil && mix.native.in.rate == s.SampleRate()
							late = 45 * time.Millisecond
						}
						if ready && wait > 5*time.Millisecond {
							p.after(wait, func() { p.post(func() { begin(buffered) }) })
							return
						}
						if !ready || wait < -late {
							p.watch.setAutomix(nil, true)
							if mix.preparedAudio(false) != nil {
								// The prepared audio is already buffered. Let its tail play under the fallback fade.
								overlap = max(trackHandoffFade, mix.overlap+wait)
							} else {
								overlap = automixOverlap
								if outgoingWatch != nil {
									outgoingWatch.setAutomix(nil, false)
								}
							}
							if atCue {
								p.out.Seek(0)
							}
							mix = &automixPlan{target: outgoingWatch.sourceTime(automixMixPosition(outgoing)), overlap: overlap}
							if p.handoff == outgoing {
								p.automixMix = mix
							}
						}
					}
					if p.handoff == outgoing && outgoing.Playing() {
						duration := overlap
						if !mix.timed() {
							end := outgoing.Duration()
							if outgoingWatch != nil {
								end = outgoingWatch.tempo.Load().mapTime(end, true)
							}
							remaining := end - automixMixPosition(outgoing)
							duration = min(overlap, max(trackHandoffFade, remaining))
							if mix != nil {
								p.automixMix = &automixPlan{target: outgoingWatch.sourceTime(automixMixPosition(outgoing)), overlap: duration}
							}
						}
						p.fade.setHandoff(outgoing, mix)
						p.fade.fade(1, duration, func() {
							p.post(func() { p.finishHandoff(outgoing) })
						})
					} else {
						p.finishHandoff(outgoing)
						p.fade.fade(1, trackHandoffFade, nil)
					}
					p.out.Play()
					p.record()
				}
				p.watch.firstRead = func(buffered bool) {
					p.post(func() { begin(buffered) })
				}
			}
			p.out = audio.NewPlayer(p.watch)
			// From its middle, as a session picked up again, it fades in.
			level := 1.0
			if p.pending > 0 || waitForFirstRead {
				level = 0
			}
			p.fade.attach(p.out, level)
			p.applyGain()
			p.out.OnEnd(func() {
				p.post(func() {
					if gen == p.gen {
						p.ended()
					}
				})
			})
			if p.pending > 0 {
				p.out.Seek(p.pending)
				p.pending = 0
			}
			if p.playing && !waitForFirstRead {
				p.out.Play()
				p.fade.fade(1, fadeIn, nil)
				p.record()
			}
			p.sync()
			p.getAhead(quality)
			p.scheduleAutomix(p.out)
		})
	}()
}

func (p *Player) playbackQuality() string {
	if p.Quality != nil {
		return p.Quality()
	}
	return music.Qualities[len(music.Qualities)/2]
}

// loadCover shows the system the cover of t, once it has it.
func (p *Player) loadCover(gen int, t *music.Track) {
	url := t.Album.ImageURL
	if url == "" {
		return
	}
	go func() {
		// The system shows it small, as Now Playing does.
		cover, err := music.Fetch(music.SizedImage(url, 600))
		if err == nil {
			p.post(func() {
				if gen == p.gen {
					p.announce(t, cover)
				}
			})
		}
	}()
}

// loadLyrics asks the source of t for its lyrics, if it has any.
func (p *Player) loadLyrics(gen int, src music.Source, t music.Track) {
	go func() {
		var lines []lyrics.Line
		if src != nil && src.Can("lyrics") {
			if l, err := src.Lyrics(t.ID); err == nil && l != nil {
				lines = lyrics.FromSource(l)
			}
		}
		p.post(func() {
			if gen == p.gen {
				p.lyrics = lines
				p.lyricsLoaded = true
			}
		})
	}()
}

// loadFailed says why the track at order[at] can't play, and skips it the way
// dir goes, unless it stays, nothing in the queue can play, or the listener has
// to do something first, which would stop the next track too.
func (p *Player) loadFailed(source string, err error, at, skipped, dir int) {
	p.err = errorText(err)
	if errors.Is(err, music.ErrSignIn) || errors.Is(err, music.ErrSetup) {
		p.playing = false
		if p.Failed != nil {
			p.Failed(source, err)
		}
		p.sync()
		return
	}
	if dir != stay && skipped+1 < len(p.order) {
		next := (at + dir + len(p.order)) % len(p.order)
		p.loadSkipping(next, skipped+1, dir, p.playing, 0)
		return
	}
	p.playing = false
	p.sync()
}

// errorText says what went wrong with a track, in the listener's terms where
// the source's error is one the app knows.
func errorText(err error) string {
	switch {
	case errors.Is(err, errNoSource):
		return i18n.T("The plugin of this song is gone")
	case errors.Is(err, errNativeCodec):
		return i18n.T("Install the MyGo native codec to play Dolby audio")
	case errors.Is(err, errCodecFailed):
		return i18n.T("Couldn't decode this audio")
	case errors.Is(err, music.ErrUnavailable):
		return i18n.T("This song isn't available")
	case errors.Is(err, music.ErrSignIn):
		return i18n.T("Sign in to play this song")
	case errors.Is(err, music.ErrSetup):
		if reason := music.Reason(err); reason != "" {
			return reason
		}
		return i18n.T("The plugin needs setting up")
	}
	return err.Error()
}

var errNoSource = errors.New("player: no such source")

// open opens track t's audio at quality: what was asked for ahead of it, if
// that worked; or kept in the cache, or else a file on disk, played from
// there, or else streamed into the cache, and kept once it has all come in.
// What failed ahead is asked for again: it may have been a moment's.
func (p *Player) open(ctx context.Context, src music.Source, t music.Track, quality string, pre *ahead) (*music.Stream, stream, error) {
	if src == nil {
		if pre != nil {
			pre.close()
		}
		return nil, nil, errNoSource
	}
	var dl *download
	var info *music.Stream
	ok := false
	if pre != nil {
		<-pre.done
		if pre.err == nil {
			info, dl, ok = pre.info, pre.dl, true
			if dl == nil {
				path, _ := music.PathOf(info.URL)
				var err error
				if dl, err = openFile(path); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	if !ok {
		dl, info, ok = p.Cache.lookup(t.Source, t.ID, quality)
	}
	if !ok {
		var err error
		info, err = src.Stream(t.ID, quality)
		if err != nil {
			return nil, nil, err
		}
		if info == nil {
			return nil, nil, music.ErrUnavailable
		}
		if path, isFile := music.PathOf(info.URL); isFile {
			dl, err = openFile(path)
			if err != nil {
				return nil, nil, err
			}
		} else {
			dl = p.Cache.download(t.Source, t.ID, quality, info)
		}
	}
	// Cancel the pending download while opening, but let successful playback own it.
	stopCancel := context.AfterFunc(ctx, dl.Close)
	s, err := openTrackStream(ctx, dl, t.Duration)
	stopCancel()
	if ctx.Err() != nil && err == nil {
		s.Close()
		return nil, nil, ctx.Err()
	}
	if err != nil {
		dl.Close()
		return nil, nil, err
	}
	return info, s, nil
}

// openFile opens a song on disk as a download that has all of it in.
func openFile(path string) (*download, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return opened(f, info.Size()), nil
}

// Session is where playback was left, to pick up again.
type Session struct {
	Queue    []music.Track `json:"queue"`
	Order    []int         `json:"order"`
	At       int           `json:"at"`
	Shuffle  bool          `json:"shuffle"`
	Repeat   RepeatMode    `json:"repeat"`
	Position int64         `json:"position"` // milliseconds
	Radio    string        `json:"radio,omitempty"`
}

// Session returns where playback is, or false with nothing queued.
func (p *Player) Session() (Session, bool) {
	if p.track == nil || p.at < 0 {
		return Session{}, false
	}
	return Session{
		Queue:    p.queue,
		Order:    p.order,
		At:       p.at,
		Shuffle:  p.shuffle,
		Repeat:   p.repeat,
		Position: p.Position().Milliseconds(),
		Radio:    p.radio,
	}, true
}

// Restore picks up a session where it was left, paused, and loads its
// track to play at once when asked.
func (p *Player) Restore(s Session) {
	n := len(s.Queue)
	if n == 0 || len(s.Order) != n || s.At < 0 || s.At >= n {
		return
	}
	seen := make([]bool, n)
	for _, i := range s.Order {
		if i < 0 || i >= n || seen[i] {
			return
		}
		seen[i] = true
	}
	p.queue, p.order = s.Queue, s.Order
	p.shuffle, p.repeat = s.Shuffle, s.Repeat%3
	p.loadSkipping(s.At, 0, stay, false, time.Duration(s.Position)*time.Millisecond)
}

// Audio is the current track's sound, opened apart from what plays by OpenAudio:
// interleaved stereo at its rate, read as it downloads.
type Audio interface {
	SampleRate() int
	Channels() int
	Read(dst []float32) (int, error)
	// SetPosition goes to a frame, of SampleRate a second.
	SetPosition(frame int64) error
	Close() error
}

// OpenAudio opens the current track's sound again, for decoding apart from what
// plays, as for transcribing an episode. It reads the same download, waiting for
// what has not come in, and fails once the track changes, or for a track that
// plays elsewhere. It works on any goroutine once opened; close it when done.
func (p *Player) OpenAudio() (Audio, error) {
	if p.stream == nil || p.track == nil {
		return nil, errors.New("player: nothing loaded")
	}
	dl, _ := p.stream.source()
	if s, ok := p.stream.(*codecPCM); ok {
		decoded, err := openShared(s.wavStream.dl, p.track.Duration)
		if err != nil {
			return nil, err
		}
		decoded.(*wavStream).downmix = s.downmix
		return decoded, nil
	}
	return openShared(dl, p.track.Duration)
}
