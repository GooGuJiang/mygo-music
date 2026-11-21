package player

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mygo-music/internal/music"
)

// Preloading yields the connection to the current track, but choosing Next fetches immediately.
func TestAheadYieldsToPlayback(t *testing.T) {
	b, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	requested := make(chan struct{}, 8)
	canceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.Write(b[:len(b)/4])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(canceled)
			return
		}
		requested <- struct{}{}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	posts, p := posted()
	t.Cleanup(p.stop)
	src := &served{url: srv.URL, asked: map[string]int{}}
	p.Source = func(string) music.Source { return src }
	p.PlayQueue([]music.Track{{ID: "a", Source: "s", Duration: 5000}, {ID: "b", Source: "s", Duration: 5000}}, 0)
	settle(t, posts, p)
	if p.ahead == nil {
		t.Fatal("next track was not queued for preload")
	}
	select {
	case <-requested:
		t.Fatal("preload competed with the active transfer")
	case <-time.After(50 * time.Millisecond):
	}
	p.Next()
	settle(t, posts, p)
	if p.Current().ID != "b" || p.Err() != "" || !p.Playing() {
		t.Fatalf("next did not promote the waiting preload: %v, %q", p.Current(), p.Err())
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("the retired player's download was still stalled")
	}
}

// A preloaded CDN URL that has expired is replaced when the track comes up.
func TestFailedAheadGetsFreshStream(t *testing.T) {
	posts, p := posted()
	t.Cleanup(p.stop)
	p.Cue([]music.Track{{ID: "a", Source: "s"}, {ID: "b", Source: "s"}})
	b, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	src := &served{url: srv.URL, asked: map[string]int{}}
	p.Source = func(string) music.Source { return src }
	p.getAhead(p.playbackQuality())
	eventually(t, "preload did not report the CDN failure", func() bool { return p.AutomixInfo().PreloadFailed })
	p.Next()
	settle(t, posts, p)
	if p.Current().ID != "b" || p.Err() != "" || src.count(src.asked, "b") != 2 || requests.Load() != 2 {
		t.Fatalf("failed preload was not replaced: %v, %q, requests=%d", p.Current(), p.Err(), requests.Load())
	}
}

// A fresh transfer gets bounded source refreshes when the CDN keeps failing.
func TestOpenRetriesTransferFailure(t *testing.T) {
	data, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= 4 {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()
	_, p := posted()
	t.Cleanup(p.stop)
	track := music.Track{ID: "b", Source: "s", Duration: 5000}
	src := &served{url: srv.URL, asked: map[string]int{}}
	p.Source = func(string) music.Source { return src }
	_, stream, err := p.open(context.Background(), src, track, p.playbackQuality(), nil)
	if err != nil {
		t.Fatalf("fresh transfer did not recover: %v", err)
	}
	stream.Close()
	if requests.Load() != 5 || src.count(src.asked, "b") != 3 {
		t.Fatalf("expected two failed downloads and one recovery, got %d HTTP requests and %d source lookups", requests.Load(), src.count(src.asked, "b"))
	}
}

// A failed preload is retried once during the current song so AutoMix can still make the handoff.
func TestAutomixRetriesFailedPreload(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		name := "recovers"
		if persistent {
			name = "stops after one retry"
		}
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/seek.mp3")
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 || persistent {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Write(data)
			}))
			defer srv.Close()
			_, p := posted()
			t.Cleanup(p.stop)
			p.Cue([]music.Track{{ID: "a", Source: "s"}, {ID: "b", Source: "s", Name: "Calling"}})
			src := &served{url: srv.URL, asked: map[string]int{}}
			p.Source = func(string) music.Source { return src }
			p.Automix = func() bool { return true }
			var tick func()
			p.after = func(_ time.Duration, fn func()) { tick = fn }
			p.Quiet = func(fn func()) { fn() }
			p.scheduleAutomix(p.out)
			p.getAhead(p.playbackQuality())
			eventually(t, "first preload did not fail", func() bool { return p.AutomixInfo().PreloadFailed })
			first := p.ahead
			tick()
			retry := p.ahead
			if retry == first {
				t.Fatal("left the failed preload in place until the song ended")
			}
			eventually(t, "retry did not settle", func() bool {
				info := p.AutomixInfo()
				return (persistent && info.PreloadFailed) || (!persistent && info.Ready)
			})
			tick()
			if p.ahead != retry || src.count(src.asked, "b") != 2 || requests.Load() != 2 {
				t.Fatalf("expected one fresh lookup, got %d lookups and %d downloads", src.count(src.asked, "b"), requests.Load())
			}
			if p.Current().ID != "a" || !p.out.Playing() {
				t.Fatal("preload interrupted the current song")
			}
		})
	}
}

// Automix waits for the incoming audio and uses an ordinary fade for PCM or an unreliable beat grid.
func TestAutomixWaitsForNextTrack(t *testing.T) {
	t.Run("PCM", func(t *testing.T) { testAutomixHandoff(t, false) })
	t.Run("Unreliable Demucs grid", func(t *testing.T) { testAutomixHandoff(t, true) })
}

func testAutomixHandoff(t *testing.T, demucs bool) {
	t.Helper()
	posts, p := posted()
	t.Cleanup(p.stop)
	p.Cue([]music.Track{{ID: "a", Source: "s"}, {ID: "b", Source: "s"}})
	p.Source = func(string) music.Source { return &served{} }
	p.Automix = func() bool { return true }
	out := p.out
	out.Pause()
	out.Seek(597700 * time.Millisecond)
	out.Play()
	quality := music.Qualities[len(music.Qualities)/2]
	if demucs {
		engine := &demucsEngine{mode: DemucsCPU}
		device := DemucsCPU
		engine.device.Store(&device)
		engine.rhythm.Store(true)
		p.stream = &demucsStream{engine: engine, cancel: func() {}}
		old, incoming := automixGrid(1100*time.Millisecond), automixGrid(1100*time.Millisecond)
		offset := out.Duration() - old.end
		for i := range old.events {
			old.events[i].at += offset
		}
		old.start, old.end = offset, out.Duration()
		incoming.events[1].at += 175 * time.Millisecond
		n := &nextRhythm{source: "s", id: "b", quality: quality, cancel: func() {}}
		n.pair.Store(&automixPair{out: old, in: incoming, stemsReady: true})
		p.nextRhythm = n
	}
	a := &ahead{source: "s", id: "b", quality: quality, done: make(chan struct{})}
	p.ahead = a
	var timer func()
	p.after = func(_ time.Duration, fn func()) { timer = fn }
	p.scheduleAutomix(out)
	if p.Current().ID != "a" || !out.Playing() || timer == nil {
		t.Fatal("Automix interrupted playback while the next track was unavailable")
	}
	data, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	a.info, a.dl = &music.Stream{Type: "mp3"}, downloaded(data)
	close(a.done)
	timer()
	next(t, posts)
	if p.Current().ID != "b" || p.handoff != out || !out.Playing() {
		t.Fatal("Automix did not keep the outgoing track during the next track's startup")
	}
	if info := p.AutomixInfo(); !info.Queued || info.Bridge || info.Beats != 0 || info.Cue != 0 {
		t.Fatalf("expected a standard fade from the incoming track's beginning: %+v", info)
	}
	settle(t, posts, p)
}

// The playback scheduler keeps native speed when it prepares a beat-guided handoff across a large BPM gap.
func TestAutomixSchedulesOriginalTempoBridge(t *testing.T) {
	_, p := posted()
	t.Cleanup(p.stop)
	p.Cue([]music.Track{{ID: "a", Source: "s"}, {ID: "b", Source: "s"}})
	p.Source = func(string) music.Source { return &served{} }
	p.Automix = func() bool { return true }
	out := p.out
	out.Pause()
	out.Seek(out.Duration() - 8*time.Second)
	out.Play()
	engine := &demucsEngine{mode: DemucsCPU}
	engine.rhythm.Store(true)
	p.stream = &demucsStream{engine: engine, cancel: func() {}}
	old := automixGrid(2640*time.Millisecond, 2640*time.Millisecond, 2640*time.Millisecond, 2640*time.Millisecond)
	in := automixGrid(1760*time.Millisecond, 1760*time.Millisecond, 1760*time.Millisecond)
	offset := out.Duration() - old.end
	for i := range old.events {
		old.events[i].at += offset
	}
	old.start, old.end = offset, out.Duration()
	quality := p.playbackQuality()
	next := &nextRhythm{source: "s", id: "b", quality: quality, cancel: func() {}}
	next.pair.Store(&automixPair{out: old, in: in, stemsReady: true})
	p.nextRhythm = next
	done := make(chan struct{})
	close(done)
	p.ahead = &ahead{source: "s", id: "b", quality: quality, done: done}
	p.after = func(time.Duration, func()) {}
	p.scheduleAutomix(out)
	info := p.AutomixInfo()
	if !info.Queued || !info.Bridge || info.Bars != 0 || out.Speed() != 1 || p.Current().ID != "a" {
		t.Fatalf("scheduler did not prepare an original-tempo bridge: %+v, speed %.3f", info, out.Speed())
	}
}

// Adding a next track during playback starts prefetch and exposes readiness before the handoff.
func TestAutomixPreloadsNewNextTrack(t *testing.T) {
	p, ch, _, q := servedQueue(t, "a", "b")
	q[1].Name = "Vagabond"
	p.PlayQueue(q[:1], 0)
	settle(t, ch, p)
	p.appendTracks(q[1:])
	if p.ahead == nil || p.ahead.id != "b" {
		t.Fatal("no prefetch was started for the appended track")
	}
	t.Cleanup(p.dropAhead)
	eventually(t, "the appended track did not become ready", func() bool { return p.AutomixInfo().Ready })
	if info := p.AutomixInfo(); info.Next != "Vagabond" || info.Preloading || info.PreloadFailed {
		t.Fatalf("unexpected prefetch status: %+v", info)
	}
}

// Overlap keeps the track endpoints, level through the midpoint, and peak headroom.
func TestHandoffGains(t *testing.T) {
	if out, in := handoffGains(0, .6, .5); out != .6 || in != 0 {
		t.Fatalf("start gains = %g, %g", out, in)
	}
	if out, in := handoffGains(1, .6, .5); out != 0 || in != .5 {
		t.Fatalf("end gains = %g, %g", out, in)
	}
	if out, in := handoffGains(.5, .6, .6); math.Abs(out*out+in*in-.36) > 1e-12 {
		t.Fatalf("midpoint lost power: %g, %g", out, in)
	}
	if out, in := handoffGains(.5, 1, 1); out+in != 1 {
		t.Fatalf("full-scale overlap can clip: %g, %g", out, in)
	}
}

// served answers each track as a song of its own address on a server, as a
// plugin does, counting what its source and the server are asked.
type served struct {
	nothing
	url string

	mu      sync.Mutex
	asked   map[string]int // streams, by track
	fetched map[string]int // requests, by path
}

func (s *served) Stream(id, _ string) (*music.Stream, error) {
	s.mu.Lock()
	s.asked[id]++
	s.mu.Unlock()
	return &music.Stream{URL: s.url + "/" + id, Type: "mp3"}, nil
}

func (s *served) count(of map[string]int, key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return of[key]
}

func servedQueue(t *testing.T, ids ...string) (*Player, chan func(), *served, []music.Track) {
	t.Helper()
	b, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	src := &served{asked: map[string]int{}, fetched: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src.mu.Lock()
		src.fetched[r.URL.Path]++
		src.mu.Unlock()
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	src.url = srv.URL
	ch := make(chan func(), 64)
	p := New(func(fn func()) { ch <- fn })
	p.SetVolume(0)
	p.Source = func(string) music.Source { return src }
	t.Cleanup(p.stop)
	var q []music.Track
	for _, id := range ids {
		q = append(q, music.Track{ID: id, Source: "s"})
	}
	return p, ch, src, q
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

// The next song is asked for once the current one plays, and comes in
// meanwhile: it then plays from that, its source asked for it once and its
// address fetched once, as a plugin's may answer only one request.
func TestNextSongComesInAhead(t *testing.T) {
	p, ch, src, q := servedQueue(t, "a", "b")
	p.PlayQueue(q, 0)
	settle(t, ch, p)
	eventually(t, "the next song didn't come in ahead", func() bool { return src.count(src.fetched, "/b") == 1 })
	p.Next()
	settle(t, ch, p)
	if p.Current().ID != "b" || p.Err() != "" || !p.Playing() {
		t.Fatalf("on %s, playing %v, err %q", p.Current().ID, p.Playing(), p.Err())
	}
	if asked, fetched := src.count(src.asked, "b"), src.count(src.fetched, "/b"); asked != 1 || fetched != 1 {
		t.Errorf("b asked for %d times, fetched %d", asked, fetched)
	}
}

// What was asked for ahead goes once it isn't next: the last song has nothing
// after it, and the first again only with the queue repeating.
func TestAheadFollowsWhatsNext(t *testing.T) {
	p, ch, src, q := servedQueue(t, "a", "b", "c")
	p.PlayQueue(q, 0)
	settle(t, ch, p)
	p.Jump(2)
	settle(t, ch, p)
	if p.ahead != nil {
		t.Errorf("still has %s ahead, after the last song", p.ahead.id)
	}
	p.CycleRepeat() // to RepeatAll
	p.Jump(1)
	settle(t, ch, p)
	p.Jump(2)
	settle(t, ch, p)
	if p.ahead == nil || p.ahead.id != "a" {
		t.Errorf("repeating, has %+v ahead", p.ahead)
	}
	// Once as it was picked, and once ahead of its turn after b, which played.
	if src.count(src.asked, "c") != 2 {
		t.Errorf("c was asked for %d times", src.count(src.asked, "c"))
	}
}
