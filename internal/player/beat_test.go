package player

import (
	"math"
	"slices"
	"testing"
	"time"
)

// Rendering a metronome tick in two decoder reads matches rendering it in one read.
func TestMetronomeClickCrossesDecoderBlocks(t *testing.T) {
	grid := &beatTimeline{events: []beatEvent{{at: time.Second, bar: true}}}
	const rate = 48000
	whole, split := make([]float32, 4000), make([]float32, 4000)
	grid.addClicks(whole, rate-100, rate, 1)
	grid.addClicks(split[:802], rate-100, rate, 1)
	grid.addClicks(split[802:], rate-100+401, rate, 1)
	if !slices.Equal(whole, split) || slices.ContainsFunc(whole[:200], func(v float32) bool { return v != 0 }) ||
		!slices.ContainsFunc(whole[200:], func(v float32) bool { return v != 0 }) {
		t.Fatal("metronome lost its source-frame alignment across the decoder boundary")
	}
}

// Changing click gain takes effect on the playing stream, and muting leaves the music untouched.
func TestMetronomeVolumeOnPlayingStream(t *testing.T) {
	music := make([]float32, 2*48000*60/1000)
	for i := range music {
		music[i] = 0.8
	}
	s := &demucsStream{}
	s.rhythm.Store(&beatTimeline{events: []beatEvent{{bar: true}}})
	p := New(func(func()) {})
	p.watch = &watched{stream: &dolbyTestStream{samples: music}, rhythm: s, now: p.now}
	read := func() []float32 {
		t.Helper()
		if err := p.watch.SetPosition(0); err != nil {
			t.Fatal(err)
		}
		pcm := make([]float32, len(music))
		if n, _ := p.watch.Read(pcm); n != len(pcm) {
			t.Fatalf("read %d samples, want %d", n, len(pcm))
		}
		return pcm
	}

	p.SetMetronome(true)
	original := read()
	p.SetMetronomeVolume(MetronomeMaxVolume)
	boosted := read()
	var peak float32
	for i, sample := range music {
		want := sample + MetronomeMaxVolume*(original[i]-sample)
		if math.Abs(float64(boosted[i]-want)) > 1e-6 || math.Abs(float64(boosted[i])) > 1 {
			t.Fatalf("boosted sample %d = %v, want %v at %dx gain", i, boosted[i], want, MetronomeMaxVolume)
		}
		peak = max(peak, sample-boosted[i])
	}
	if peak < 1.4 {
		t.Fatalf("boosted click barely breaks through loud music: peak difference %v", peak)
	}
	p.SetMetronomeVolume(0)
	if !slices.Equal(read(), music) {
		t.Fatal("muting the metronome changed the music")
	}
	p.SetMetronome(false)
	p.SetMetronomeVolume(MetronomeMaxVolume)
	if !slices.Equal(read(), music) {
		t.Fatal("changing gain turned the metronome back on")
	}
}
