package player

import (
	"testing"
	"time"

	"mygo-music/internal/music"
)

// A 90-to-135 BPM transition skips the clashing intro, loops one or two complete bars, and requires separated audio.
func TestAutomixLoopSelectsLocalHarmony(t *testing.T) {
	const oldBar, nextBar = 2640 * time.Millisecond, 1760 * time.Millisecond
	out := automixGrid(oldBar, oldBar, oldBar, oldBar, oldBar, oldBar)
	in := automixGrid(nextBar, nextBar, nextBar, nextBar)
	c, clash := [12]float64{0: .5, 4: .25, 7: .25}, [12]float64{6: .5, 10: .25, 1: .25}
	for _, grid := range []*beatTimeline{out, in} {
		grid.stems = make([]automixStem, int(grid.end/automixFeatureStep))
		for i := range grid.stems {
			grid.stems[i] = automixStem{known: true, chroma: c, power: [demucsStemCount]float64{0, .02, .01, .02}}
		}
	}
	for i := range int(nextBar/automixFeatureStep) + 1 {
		in.stems[i].chroma = clash
	}
	cut := planAutomixLoop(out, in, out.end)
	if cut.end-cut.start != oldBar || cut.cue != nextBar || cut.target() != 5*oldBar {
		t.Fatalf("selected the wrong loop or incoming downbeat: %+v", cut)
	}
	for i := int(cut.start / automixFeatureStep); i < int(cut.end/automixFeatureStep); i++ {
		out.stems[i].vocal = .85
	}
	if alternate := planAutomixLoop(out, in, out.end); alternate.end-alternate.start != 2*oldBar || alternate.cue != nextBar {
		t.Fatalf("voice-heavy bar blocked a clean two-bar loop: %+v", alternate)
	}
	for i := range in.stems {
		in.stems[i].chroma = clash
	}
	if cut := planAutomixLoop(out, in, out.end); cut.end != 0 {
		t.Fatalf("looped across incompatible harmony: %+v", cut)
	}
	in.stems = nil
	if cut := planAutomixLoop(out, in, out.end); cut.end != 0 {
		t.Fatalf("looped without separated features: %+v", cut)
	}
}

// The outgoing stream repeats the drums at original speed, ducks the voice, and leaves only a short tonal tail at the handoff.
func TestAutomixLoopPlaysSeparatedRepeat(t *testing.T) {
	const rate = 48000
	cut := automixLoopCut{start: time.Second, end: 3 * time.Second, voice: 400 * time.Millisecond}
	dry := make([]float32, 2*6*rate)
	split := make([][2 * demucsOther]float32, len(dry)/2)
	for i := range split {
		drum := float32(.125)
		if i >= rate+rate/4 && i < rate+rate/3 {
			drum += .25
		}
		split[i] = [2 * demucsOther]float32{.25, 0, drum, 0, .25, 0}
		dry[2*i], dry[2*i+1] = .5+drum, .5
	}
	loop := renderAutomixLoop(cut, &beatTimeline{}, rate, 0, dry, split)
	if loop == nil {
		t.Fatal("no audio for a fully analyzed repeat")
	}
	for _, withDolby := range []bool{false, true} {
		s := &demucsStream{stream: &dolbyTestStream{samples: dry}}
		s.wantedMix.Store(&demucsGains{demucsDrums: -.5})
		var stream stream = s
		if withDolby {
			stream = &dolbyStream{stream: s}
		}
		w := &watched{stream: stream, rhythm: s, now: time.Now}
		w.setAutomix(&automixPlan{bridge: true, loop: loop}, false)
		read := func(frame int64) [2]float32 {
			t.Helper()
			if err := w.SetPosition(frame); err != nil {
				t.Fatal(err)
			}
			var got [2]float32
			if n, err := w.Read(got[:]); n != len(got) || err != nil {
				t.Fatalf("read %d samples: %v", n, err)
			}
			return got
		}
		if got := read(2 * rate); got != [2]float32{.625, .5} {
			t.Fatalf("changed the song before the loop: %v", got)
		}
		if got := read(3*rate - 1); got != [2]float32{.3125, .5} || got != read(3*rate) {
			t.Fatalf("vocal leaked or the loop clicked at the seam: %v", got)
		}
		if got := read(3*rate + rate/4); got != [2]float32{.4375, .5} {
			t.Fatalf("repeat lost the original drum timing or stem gain: %v", got)
		}
		if got := read(5 * rate); got[0] != 0 || got[1] < .139 || got[1] > .141 {
			t.Fatalf("outgoing drums or bass still audible on the incoming downbeat: %v", got)
		}
		if got := read(5*rate + rate/2); got != [2]float32{} {
			t.Fatalf("outgoing audio came back after the overlap: %v", got)
		}
		w.setAutomix(nil, false)
		if got := read(3 * rate); got != [2]float32{.625, .5} {
			t.Fatalf("canceling the loop did not restore the normal stream: %v", got)
		}
	}
}

// After the repeated bar, the loop exits into the natural playhead instead of replaying the OUT tail.
func TestAutomixLoopResumesNaturalPlayhead(t *testing.T) {
	const rate = 48000
	cut := automixLoopCut{start: time.Second, end: 3 * time.Second, voice: 400 * time.Millisecond}
	dry := make([]float32, 2*7*rate)
	for frame := range len(dry) / 2 {
		dry[2*frame], dry[2*frame+1] = float32(frame)/rate, float32(frame)/rate
	}
	split := make([][2 * demucsOther]float32, len(dry)/2)
	source := automixStemSource{timeline: &beatTimeline{}, rate: rate, dry: dry, split: split}
	begin, pivot := cut.end-cut.voice, cut.target()
	loop := source.render(begin, pivot+automixLoopTail, cut, false, automixLowpassTail{}, func(time.Duration) [demucsStemCount]float32 {
		return [demucsStemCount]float32{demucsOther: 1}
	})
	if loop == nil {
		t.Fatal("no audio for a fully analyzed repeat")
	}
	frame := func(at time.Duration) float32 {
		return loop.frames[int((at-begin)*rate/time.Second)][2*demucsOther]
	}
	if got := frame(pivot + 100*time.Millisecond); got <= 2.1 {
		t.Fatalf("loop exit still replayed the OUT tail: %f", got)
	}
}

// The playback scheduler arms a prepared repeat before it reaches the decoder and keeps the outgoing speed at unity.
func TestAutomixSchedulesPreparedLoop(t *testing.T) {
	_, p := posted()
	t.Cleanup(p.stop)
	p.Cue([]music.Track{{ID: "a", Source: "s"}, {ID: "b", Source: "s"}})
	p.Source = func(string) music.Source { return &served{} }
	p.Automix = func() bool { return true }
	out := p.out
	out.Pause()
	out.Seek(out.Duration() - 9*time.Second)
	out.Play()
	engine := &demucsEngine{}
	engine.rhythm.Store(true)
	s := &demucsStream{stream: &dolbyTestStream{}, engine: engine, cancel: func() {}}
	p.stream, p.watch = s, &watched{stream: s, rhythm: s}
	old := automixGrid(2640*time.Millisecond, 2640*time.Millisecond, 2640*time.Millisecond)
	in := automixGrid(1760*time.Millisecond, 1760*time.Millisecond)
	offset := out.Duration() - old.end
	for i := range old.events {
		old.events[i].at += offset
	}
	old.start, old.end = offset, out.Duration()
	cut := automixLoopCut{start: old.events[0].at, end: old.events[4].at, voice: 500 * time.Millisecond, cue: in.events[4].at}
	loop := &automixLoopAudio{cut: cut, rate: s.SampleRate()}
	quality := p.playbackQuality()
	next := &nextRhythm{source: "s", id: "b", quality: quality, cancel: func() {}}
	next.pair.Store(&automixPair{out: old, in: in, loop: loop, stemsReady: true})
	p.nextRhythm = next
	done := make(chan struct{})
	close(done)
	p.ahead = &ahead{source: "s", id: "b", quality: quality, done: done}
	p.after = func(time.Duration, func()) {}
	p.scheduleAutomix(out)
	if info := p.AutomixInfo(); !info.Queued || info.Start != cut.target() || info.Cue != cut.cue || s.automixLoop.Load() != loop || out.Speed() != 1 {
		t.Fatalf("did not arm the native-speed loop: %+v at %.3f", info, out.Speed())
	}
}
