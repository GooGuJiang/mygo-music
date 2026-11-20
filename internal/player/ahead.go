package player

import (
	"context"
	"math"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/audio"
	"mygo-music/internal/music"
)

const (
	automixOverlap = 2500 * time.Millisecond
	automixLead    = 850 * time.Millisecond
	automixAhead   = 250 * time.Millisecond // audio.LatencyPlayback
)

type nextRhythm struct {
	source, id, quality string
	cancel              context.CancelFunc
	pair                atomic.Pointer[automixPair]
}

func (p *Player) clearNextRhythm() {
	if p.nextRhythm != nil {
		p.nextRhythm.cancel()
		p.nextRhythm = nil
	}
}

func (p *Player) prepareNextRhythm(out *demucsStream, quality string, end time.Duration) {
	t, ok := p.upNext()
	if !ok {
		p.clearNextRhythm()
		return
	}
	if n := p.nextRhythm; n != nil && n.source == t.Source && n.id == t.ID && n.quality == quality {
		return
	}
	p.clearNextRhythm()
	var dl *download
	if a := p.ahead; a != nil && a.is(t, quality) && a.err == nil {
		if dl = a.dl; dl != nil {
			if !dl.share() {
				return
			}
		} else if path, ok := music.PathOf(a.info.URL); ok {
			dl, _ = openFile(path)
		}
	} else {
		dl, _, _ = p.Cache.lookup(t.Source, t.ID, quality)
	}
	if dl == nil {
		return
	}
	outDL, _ := out.source()
	if outDL == nil || outDL.Progress() < 1 || !outDL.share() {
		dl.release()
		return
	}
	engine, duration := out.engine, p.track.Duration
	ctx, cancel := context.WithCancel(context.Background())
	n := &nextRhythm{source: t.Source, id: t.ID, quality: quality, cancel: cancel}
	p.nextRhythm = n
	engine.retain()
	go func() {
		defer engine.release()
		defer dl.release()
		defer outDL.release()
		in, err := openShared(dl, t.Duration)
		if err != nil {
			return
		}
		defer in.Close()
		old, err := openShared(outDL, duration)
		if err != nil {
			return
		}
		defer old.Close()
		analyzeAutomixPair(ctx, engine, old, in, end, n.pair.Store)
	}()
}

// AutomixActive reports actual overlap, including neither waiting nor manual skips.
func (p *Player) AutomixActive() bool {
	return p.automixMix != nil && p.handoff != nil && p.out != nil && p.out.Playing()
}

func (p *Player) scheduleAutomix(out *audio.Player) {
	if p.Automix == nil || p.track != nil && p.track.ShowID != "" || out.Duration() < 2*automixOverlap {
		return
	}
	s, _ := p.stream.(*demucsStream)
	post := p.Quiet
	if post == nil {
		post = p.post
	}
	missedCue := false
	var check func()
	check = func() {
		if p.out != out {
			return
		}
		position := out.Position()
		remaining := out.Duration() - position
		if remaining <= 0 {
			p.cancelAutomixPrep()
			return
		}
		wait := min(time.Second, max(150*time.Millisecond, remaining-5*time.Second))
		if speed := out.Speed(); missedCue && speed != 1 {
			out.SetSpeed(stepAutomixSpeed(speed, 1, automixTempoTick))
			wait = min(wait, automixTempoTick)
		}
		quality := p.playbackQuality()
		if p.ahead != nil && p.ahead.quality != quality {
			p.refreshAhead()
		}
		enabled := p.Automix() && (p.Dolby == nil || !p.Dolby().Spatial)
		if !enabled {
			p.clearNextRhythm()
		}
		lookahead := 45 * time.Second
		if s != nil && s.engine != nil {
			if device := s.engine.device.Load(); device != nil && *device == DemucsCPU {
				lookahead = 2 * time.Minute
			}
		}
		if enabled && remaining <= lookahead && p.playing && out.Playing() && !p.seeking && !p.Loading() && p.handoff == nil && p.automixReady(quality) {
			if s != nil && s.engine != nil && !s.failed.Load() && s.engine.rhythm.Load() {
				p.prepareNextRhythm(s, quality, out.Duration())
				if prep := p.automixPrep; prep != nil && prep.next != p.nextRhythm {
					p.cancelAutomixPrep()
				}
				if p.automixPrep == nil && p.nextRhythm != nil && !missedCue {
					if pair := p.nextRhythm.pair.Load(); pair != nil {
						var plan automixPlan
						if native := pair.native; native != nil && p.watch != nil && p.watch.rhythm == s && native.out.rate == p.watch.SampleRate() &&
							position+automixLoopDecodeLead < time.Duration(float64(native.out.first)*float64(time.Second)/float64(native.out.rate)) {
							cut := native.cut
							plan = automixPlan{bridge: true, target: cut.outBeat - cut.lead, cue: cut.inBeat - cut.lead,
								overlap: cut.lead + cut.tail, pivot: cut.lead, native: native}
						} else if plan = matchBars(pair.out, pair.in, out.Duration(), position); plan.count == 0 {
							if loop := pair.loop; loop != nil && p.watch != nil && p.watch.rhythm == s && loop.rate == p.watch.SampleRate() &&
								position+automixLoopDecodeLead < loop.cut.end-loop.cut.voice {
								plan = automixPlan{bridge: true, cue: loop.cut.cue, target: loop.cut.target(), overlap: automixLoopTail, loop: loop}
							} else {
								plan = planAutomixBridge(pair.out, pair.in, out.Duration(), position)
							}
						}
						// Give stem analysis time to finish without missing a usable downbeat.
						rampDue := plan.count > 0 && plan.bars[0].speed != 1 && plan.rampStart-position <= time.Second
						if plan.timed() && (plan.native != nil || plan.loop != nil && pair.nativeReady || pair.stemsReady || rampDue || plan.target-position <= 5*time.Second) {
							if !pair.stemsReady {
								p.nextRhythm.cancel()
							}
							p.automixPrep = &automixPreparation{plan: plan, next: p.nextRhythm, out: out, watch: p.watch}
							if p.watch != nil {
								p.watch.setAutomix(&plan, false)
							}
						}
					}
				}
			} else {
				p.cancelAutomixPrep()
			}
			if prep := p.automixPrep; prep != nil {
				position = automixMixPosition(out)
				plan := &prep.plan
				delta := plan.target - position
				speed, next := plan.approach(position)
				switch {
				case delta > automixLead:
					if out.Speed() != speed {
						out.SetPreservesPitch(true).SetSpeed(speed)
					}
					wait = min(wait, time.Duration(float64(min(next-position, delta-automixLead))/speed))
				case delta > 80*time.Millisecond && math.Abs(out.Speed()-speed) <= .02:
					p.automixPrep = nil // handoff takes ownership of the running transition
					p.loadTrack((p.at+1)%len(p.order), 0, forward, true, plan.cue, plan.overlap, plan)
					return
				default:
					// Keep playing the current track if we missed the cue. Give its tempo time to settle.
					missedCue, p.automixPrep = true, nil
					if prep.watch != nil {
						prep.watch.setAutomix(nil, false)
					}
					wait = min(wait, automixTempoTick)
				}
			}
			if p.automixPrep == nil && remaining <= automixOverlap {
				p.loadTrack((p.at+1)%len(p.order), 0, forward, true, 0, remaining, &automixPlan{target: position})
				return
			}
		} else {
			p.cancelAutomixPrep()
		}
		p.after(wait, func() { post(check) })
	}
	check()
}

func (p *Player) automixReady(quality string) bool {
	t, ok := p.upNext()
	if !ok || t.ShowID != "" {
		return false
	}
	src := p.sourceOf(&t)
	if src == nil || src.Info().Playback == music.PlaybackRemote {
		return false
	}
	if a := p.ahead; a != nil && a.is(t, quality) {
		ready, _, _ := a.transfer()
		return ready
	}
	return p.Cache.has(t.Source, t.ID, quality)
}

// The next track's audio is asked for once the current one plays, and comes in
// meanwhile, so that it starts at once when its turn comes: a source may take
// seconds to give a stream, as one that works it out with programs does.

// ahead is a track's audio asked for before its turn.
type ahead struct {
	source, id, quality string
	done                chan struct{} // closed once info, dl and err are in
	info                *music.Stream
	dl                  *download // nil for a song on disk, which opens at once
	err                 error
}

func (a *ahead) is(t music.Track, quality string) bool {
	return a.source == t.Source && a.id == t.ID && a.quality == quality
}

func (a *ahead) transfer() (ready, failed bool, progress float64) {
	select {
	case <-a.done:
	default:
		return false, false, 0
	}
	if a.err != nil {
		return false, true, 0
	}
	if a.dl == nil {
		return true, false, 1 // file URL
	}
	d := a.dl
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.size > 0 {
		progress = min(1, float64(d.n)/float64(d.size))
	}
	return d.done && d.err == nil, d.done && d.err != nil, progress
}

func (p *Player) refreshAhead() {
	if p.out != nil {
		if t, ok := p.upNext(); p.nextRhythm != nil && (!ok || p.nextRhythm.source != t.Source || p.nextRhythm.id != t.ID) {
			p.cancelAutomixPrep()
			p.clearNextRhythm()
		}
		p.getAhead(p.playbackQuality())
	}
}

// upNext is the track that plays when the current one ends, if one does by
// itself: not a radio's next, which its feed may not have given yet.
func (p *Player) upNext() (music.Track, bool) {
	if p.at < 0 || p.at >= len(p.order) || p.repeat == RepeatOne {
		return music.Track{}, false
	}
	at := p.at + 1
	if at >= len(p.order) {
		if p.repeat != RepeatAll || p.feed != nil {
			return music.Track{}, false
		}
		at = 0
	}
	if at == p.at {
		return music.Track{}, false
	}
	return p.queue[p.order[at]], true
}

// getAhead asks for the next track's audio at quality, unless it's kept
// already or its source plays it elsewhere. What was asked for before that
// isn't next any more is let go.
func (p *Player) getAhead(quality string) {
	t, ok := p.upNext()
	if a := p.ahead; a != nil {
		if ok && a.is(t, quality) {
			return
		}
		p.dropAhead()
	}
	if !ok {
		return
	}
	src := p.sourceOf(&t)
	if src == nil || src.Info().Playback == music.PlaybackRemote {
		return
	}
	a := &ahead{source: t.Source, id: t.ID, quality: quality, done: make(chan struct{})}
	p.ahead = a
	go func() {
		defer close(a.done)
		if dl, info, ok := p.Cache.lookup(t.Source, t.ID, quality); ok {
			a.dl, a.info = dl, info
			return
		}
		info, err := src.Stream(t.ID, quality)
		if err == nil && info == nil {
			err = music.ErrUnavailable
		}
		if err != nil {
			a.err = err
			return
		}
		a.info = info
		if _, isFile := music.PathOf(info.URL); !isFile {
			a.dl = p.Cache.download(t.Source, t.ID, quality, info)
		}
	}()
}

// takeAhead returns what was asked for ahead of t at quality, if it was, for
// the caller to open or close.
func (p *Player) takeAhead(t music.Track, quality string) *ahead {
	a := p.ahead
	if a == nil || !a.is(t, quality) {
		return nil
	}
	p.ahead = nil
	return a
}

// dropAhead lets go of what was asked for ahead, stopping it if it's still
// coming in. What has all come in stays in the cache.
func (p *Player) dropAhead() {
	a := p.ahead
	p.ahead = nil
	if a == nil {
		return
	}
	go a.close()
}

// close stops what was asked for ahead once it's in, if it's still coming in.
func (a *ahead) close() {
	<-a.done
	if a.dl != nil {
		a.dl.Close()
	}
}
