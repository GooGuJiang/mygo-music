package player

import (
	"io"
	"math"
	"slices"
	"testing"
)

// Compensation follows the delayed Demucs audio, including after a seek, and leaves fallback frames untouched.
func TestDemucsClarityFollowsRenderedBed(t *testing.T) {
	const delay, start, wetFrames, total = 254, 900, 8000, 10000
	dry := make([]float32, 2*total)
	bed := make([]float32, demucsFrameSamples*wetFrames)
	for i := range total {
		x := float32(0.4 * math.Sin(2*math.Pi*5813*float64(i)/48000))
		dry[i*2], dry[i*2+1] = x, x/2
		if i >= start && i < start+wetFrames {
			j := (i - start) * demucsFrameSamples
			bed[j], bed[j+1] = x, x/2
		}
	}
	source := &demucsStream{
		stream:  &dolbyTestStream{samples: dry},
		cancel:  func() {},
		results: make(chan demucsBlock, 1),
		wake:    make(chan struct{}, 1),
	}
	source.request.Store(&demucsRequest{})
	opens := 0
	s := &dolbyStream{stream: source, openEffect: func(_, _ int, _ DolbyProfile) (dolbyEffect, int) {
		opens++
		return &delayedBed{delayedEffect{delay: make([]float32, delay), gain: 1}}, delay
	}}
	defer s.Close()
	s.requested.Store(&DolbyProfile{Spatial: true})
	source.results <- demucsBlock{request: source.request.Load(), start: start, bed: bed}
	s.clarityOn.Store(true)
	read := func() []float32 {
		t.Helper()
		var out []float32
		buf := make([]float32, 272)
		for {
			n, err := s.Read(buf)
			out = append(out, buf[:n]...)
			if err == io.EOF {
				return out
			}
			if err != nil || n == 0 {
				t.Fatalf("Read = %d, %v", n, err)
			}
		}
	}
	out := read()
	if len(out) != len(dry) {
		t.Fatalf("returned %d samples, want %d", len(out), len(dry))
	}
	if !slices.Equal(out[:2*start], dry[:2*start]) || !slices.Equal(out[2*(start+wetFrames-1):], dry[2*(start+wetFrames-1):]) {
		t.Fatal("compensation reached frames outside the Demucs bed")
	}
	if slices.Equal(out[2*(start+4000):2*(start+4500)], dry[2*(start+4000):2*(start+4500)]) {
		t.Fatal("compensation never reached the rendered bed")
	}
	if err := s.SetPosition(start); err != nil {
		t.Fatal(err)
	}
	if got := read(); !slices.Equal(got, dry[2*start:]) || opens != 1 {
		t.Fatal("seek kept the compensation from the old bed or reopened the renderer")
	}
}

type delayedBed struct{ delayedEffect }

func (e *delayedBed) render(bed, stereo []float32) bool {
	for i := 0; i < len(stereo)/2; i++ {
		copy(stereo[2*i:2*i+2], bed[i*atmosPCMChannels:])
	}
	return e.process(stereo)
}

// A bright signal at full scale must stay below clipping without pulling the stereo image to one side.
func TestDemucsClarityStereoHeadroom(t *testing.T) {
	for _, rate := range []int{32000, 44100, 48000, 192000} {
		eq := newDemucsClarity(rate)
		frames := rate / 6
		weights := make([]float32, frames)
		pcm := make([]float32, 2*frames)
		for i := range weights {
			weights[i] = 1
			x := float32(math.Sin(2 * math.Pi * 9000 * float64(i) / float64(rate)))
			pcm[2*i], pcm[2*i+1] = x, x/2
		}
		first := pcm[2]
		eq.process(pcm, weights, true)
		if math.Abs(float64(pcm[2]-first)) > 0.001 {
			t.Fatalf("%d Hz: toggle introduced a step", rate)
		}
		for i := range weights {
			l, r := pcm[2*i], pcm[2*i+1]
			if math.IsNaN(float64(l)) || l < -1 || l > 1 || l/2 != r {
				t.Fatalf("%d Hz, frame %d: unsafe stereo sample [%g %g]", rate, i, l, r)
			}
			if i >= rate*dolbyFadeMillis/1000 && math.Abs(float64(l)) > 0.842 {
				t.Fatalf("%d Hz, frame %d: peaked above the steady-state ceiling: %g", rate, i, l)
			}
		}
	}
}
