package player

import (
	"io"
	"slices"
	"testing"
)

type dolbyTestStream struct {
	stream
	samples []float32
	at      int
	seeks   int
}

func (s *dolbyTestStream) Read(dst []float32) (int, error) {
	n := copy(dst, s.samples[s.at:])
	s.at += n
	if s.at == len(s.samples) {
		return n, io.EOF
	}
	return n, nil
}

func (s *dolbyTestStream) SetPosition(frame int64) error {
	s.seeks++
	s.at = int(frame) * 2
	return nil
}

func (s *dolbyTestStream) SampleRate() int { return 48000 }
func (s *dolbyTestStream) Channels() int   { return 2 }
func (s *dolbyTestStream) Close() error    { return nil }

type delayedEffect struct {
	delay []float32
	at    int
	gain  float32
}

func (e *delayedEffect) process(samples []float32) bool {
	for i, x := range samples {
		samples[i], e.delay[e.at] = e.delay[e.at], x*e.gain
		e.at = (e.at + 1) % len(e.delay)
	}
	return true
}

func (e *delayedEffect) reset() bool { clear(e.delay); return true }
func (e *delayedEffect) close()      {}

// A delayed processor must keep the end of the song and play the right frames after a seek.
func TestDolbyStreamKeepsFrames(t *testing.T) {
	const delay = 6
	s := &dolbyStream{
		stream: &dolbyTestStream{samples: []float32{1, -1, 2, -2, 3, -3, 4, -4}},
		openEffect: func(int, int, DolbyProfile) (dolbyEffect, int) {
			return &delayedEffect{delay: make([]float32, delay), gain: 2}, delay
		},
	}
	s.requested.Store(&DolbyProfile{Leveler: 1})
	defer s.Close()
	for _, seek := range []int64{0, 3, 4, 1} {
		if err := s.SetPosition(seek); err != nil {
			t.Fatal(err)
		}
		var got []float32
		var buf [4]float32
		for {
			n, err := s.Read(buf[:])
			got = append(got, buf[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil || n == 0 {
				t.Fatalf("seek %d: Read = %d, %v", seek, n, err)
			}
		}
		want := []float32{2, -2, 4, -4, 6, -6, 8, -8}[2*seek:]
		if !slices.Equal(got, want) {
			t.Errorf("seek %d: got %v, want %v", seek, got, want)
		}
	}
}

// Switching between two processor delays and bypass mixed mismatched frames if the paths lost alignment.
func TestDolbyCrossfadeKeepsFrameOrder(t *testing.T) {
	const total = 12000
	const fade = 48000 * dolbyFadeMillis / 1000
	dry := make([]float32, total*2)
	for frame := range total {
		x := float32(frame+1) / total / 4
		dry[frame*2], dry[frame*2+1] = x, -x
	}
	source := &dolbyTestStream{samples: dry}
	s := &dolbyStream{stream: source, openEffect: func(_, _ int, profile DolbyProfile) (dolbyEffect, int) {
		delay := 6
		if profile.Leveler == 2 {
			delay = 5000
		}
		return &delayedEffect{delay: make([]float32, delay), gain: float32(profile.Leveler + 1)}, delay
	}}
	defer s.Close()
	s.requested.Store(&DolbyProfile{Leveler: 1})
	var got []float32
	var block [512]float32
	var switches []int
	for {
		if len(switches) < 2 && len(got)/2 >= 1024+len(switches)*4096 {
			switches = append(switches, len(got)/2)
			level := 0.0
			if len(switches) == 1 {
				level = 2
			}
			s.requested.Store(&DolbyProfile{Leveler: level})
		}
		n, err := s.Read(block[:])
		got = append(got, block[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil || n == 0 {
			t.Fatalf("Read = %d, %v", n, err)
		}
	}
	if source.seeks != 0 || len(got) != len(dry) || len(switches) != 2 {
		t.Fatalf("got %d samples and %d seeks with switches at %v", len(got), source.seeks, switches)
	}
	for frame := range total {
		gain := float32(2)
		for i, at := range switches {
			if frame < at {
				break
			}
			x := float32(min(frame-at, fade)) / fade
			gain += [...]float32{1, -2}[i] * x * x * (3 - 2*x)
		}
		for ch := range 2 {
			i := frame*2 + ch
			if diff := got[i] - dry[i]*gain; diff < -0.000001 || diff > 0.000001 {
				t.Fatalf("frame %d: got %f, want %f", frame, got[i], dry[i]*gain)
			}
		}
	}
}
