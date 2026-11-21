//go:build windows && amd64

package player

import (
	"io"
	"math"
	"os"
	"testing"
	"time"

	"mygo-music/internal/music"
)

type automixTestPCM struct {
	stream
	samples []float32
	frame   int
}

func (*automixTestPCM) SampleRate() int { return 48000 }
func (*automixTestPCM) Channels() int   { return 2 }
func (*automixTestPCM) Close() error    { return nil }
func (s *automixTestPCM) Len() int64    { return int64(len(s.samples) / 2) }
func (s *automixTestPCM) SetPosition(frame int64) error {
	s.frame = int(frame)
	return nil
}

func (s *automixTestPCM) Read(dst []float32) (int, error) {
	if s.frame == len(s.samples)/2 {
		return 0, io.EOF
	}
	n := copy(dst, s.samples[2*s.frame:])
	s.frame += n / 2
	return n, nil
}

func requireRubberBand(t *testing.T) {
	t.Helper()
	if loadRubberBand() == nil {
		if os.Getenv("MYGO_MUSIC_TEST_RUBBERBAND") != "" {
			t.Fatal("could not load the packaged Rubber Band engine")
		}
		t.Skip("build the Windows x64 Rubber Band engine to exercise the native decoder")
	}
}

// Windows release audio preserves pitch and keeps transients near the planned cues after a variable stretch.
func TestAutomixRubberBandKeepsPitchAndCue(t *testing.T) {
	requireRubberBand(t)
	for _, test := range []struct {
		name string
		bars [automixBars]automixBar
	}{
		{"faster", [automixBars]automixBar{{out: 2200 * time.Millisecond, in: 2 * time.Second, speed: 1.1}, {out: 2140 * time.Millisecond, in: 2 * time.Second, speed: 1.07}}},
		{"slower", [automixBars]automixBar{{out: 1800 * time.Millisecond, in: 2 * time.Second, speed: .9}, {out: 1860 * time.Millisecond, in: 2 * time.Second, speed: .93}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const rate = 48000
			plan := &automixPlan{rampStart: time.Second, target: 5 * time.Second, count: 2, bars: test.bars}
			tempo := newAutomixTempo(plan, rate)
			input := &automixTestPCM{samples: make([]float32, 2*10*rate)}
			frameAt := func(d time.Duration) int { return int(math.Round(d.Seconds() * rate)) }
			for frame := 0; frame < len(input.samples)/2; frame++ {
				input.samples[2*frame] = .18 * float32(math.Sin(2*math.Pi*440*float64(frame)/rate+.43))
			}
			beats := []time.Duration{plan.target, plan.target + test.bars[0].out, plan.outEnd()}
			for _, beat := range beats {
				for frame := range rate / 200 {
					input.samples[2*(frameAt(beat)+frame)+1] = .7 * float32(math.Sin(2*math.Pi*1600*float64(frame)/rate))
				}
			}
			watch := &watched{stream: input, now: time.Now}
			watch.tempo.Store(tempo)
			t.Cleanup(func() { watch.Close() })
			var output []float32
			var block [4096]float32
			for {
				n, err := watch.Read(block[:])
				output = append(output, block[:n]...)
				if err == io.EOF {
					break
				}
				if err != nil || n == 0 {
					t.Fatalf("native decoder returned %d samples and %v", n, err)
				}
			}
			wantLength := frameAt(tempo.mapTime(10*time.Second, true))
			if got := len(output) / 2; got > wantLength || got < wantLength-rate/100 {
				t.Fatalf("output length %d frames, want %d", got, wantLength)
			}
			if seam := 2 * tempo.first; output[seam] != input.samples[seam] {
				t.Fatalf("first stretched sample jumped from %.4f to %.4f", input.samples[seam], output[seam])
			}
			start := frameAt(tempo.mapTime(plan.target, true)) + rate/4
			var crossings int
			for i := start + 1; i < start+rate; i++ {
				if output[2*i] >= 0 && output[2*i-2] < 0 {
					crossings++
				}
			}
			if crossings < 438 || crossings > 442 {
				t.Fatalf("440 Hz tone changed to about %d Hz", crossings)
			}
			for _, beat := range beats {
				want := frameAt(tempo.mapTime(beat, true))
				peak := want
				for i := want - rate/10; i < want+rate/10; i++ {
					if math.Abs(float64(output[2*i+1])) > math.Abs(float64(output[2*peak+1])) {
						peak = i
					}
				}
				shift := time.Duration(peak-want) * time.Second / rate
				t.Logf("cue %s: transient offset %s", beat, shift)
				if shift.Abs() > 25*time.Millisecond || math.Abs(float64(output[2*peak+1])) < .2 {
					t.Errorf("transient at source %s missed the scheduled cue or lost its attack", beat)
				}
			}
			watch.tempo.Store(nil)
			const seek = rate*3 + 53
			if err := watch.SetPosition(seek); err != nil {
				t.Fatal(err)
			}
			if n, err := watch.Read(block[:2]); err != nil || n != 2 || block[0] != input.samples[2*seek] {
				t.Fatalf("seek kept the stretched audio: read %d, %v", n, err)
			}
		})
	}
}

// A moderate BPM change arms the decoder's native engine, and disabling AutoMix disarms it.
func TestAutomixRubberBandScheduler(t *testing.T) {
	requireRubberBand(t)
	_, p := posted()
	t.Cleanup(p.stop)
	p.Quiet = func(fn func()) { fn() }
	p.Cue([]music.Track{{ID: "a", Source: "s"}, {ID: "b", Source: "s"}})
	p.Source = func(string) music.Source { return &served{} }
	enabled := true
	p.Automix = func() bool { return enabled }
	out := p.out
	out.Pause()
	out.Seek(out.Duration() - 15*time.Second)
	out.Play()
	engine := &demucsEngine{}
	engine.rhythm.Store(true)
	s := &demucsStream{stream: &dolbyTestStream{}, engine: engine, cancel: func() {}}
	p.stream, p.watch = s, &watched{stream: s, rhythm: s}
	old := automixGrid(2200*time.Millisecond, 2200*time.Millisecond, 2200*time.Millisecond, 2200*time.Millisecond)
	in := automixGrid(2*time.Second, 2*time.Second, 2*time.Second, 2*time.Second)
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
	var check func()
	p.after = func(_ time.Duration, fn func()) { check = fn }
	p.scheduleAutomix(out)
	if p.automixPrep == nil || p.watch.tempo.Load() == nil || out.PreservesPitch() || out.Speed() != 1 {
		t.Fatal("scheduler did not route the BPM change through Rubber Band")
	}
	enabled = false
	check()
	if p.automixPrep != nil || p.watch.tempo.Load() != nil {
		t.Fatal("disabling AutoMix left the tempo change armed")
	}
}
