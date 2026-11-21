package player

import (
	"io"
	"math"
	"sync/atomic"
	"time"
)

type automixTempoPoint struct{ source, playback time.Duration }

const automixTempoPreroll = 150 * time.Millisecond

type automixTempo struct {
	points  []automixTempoPoint
	first   int64
	started atomic.Bool
	failed  atomic.Bool
}

func (m *automixPlan) changesTempo() bool {
	for _, bar := range m.bars[:m.count] {
		if bar.out != bar.in || bar.outSplit != bar.inSplit {
			return true
		}
	}
	return false
}

func newAutomixTempo(m *automixPlan, rate int) *automixTempo {
	// Start at unity with room for the stretcher's first analysis window.
	first := max(0, m.rampStart-automixTempoPreroll)
	frame := tempoTimeFrame(first, rate)
	first = tempoFrameTime(frame, rate)
	warp := &automixTempo{first: frame, points: []automixTempoPoint{{first, first}}}
	add := func(source, playback time.Duration) {
		warp.points = append(warp.points, automixTempoPoint{source, playback})
	}
	start := m.rampStart
	if start > first {
		add(start, start)
	}
	playback := max(first, start)
	for at := playback; at < m.target; {
		next := min(at+automixTempoTick, m.target)
		speed, _ := m.approach(at + (next-at)/2)
		playback += time.Duration(float64(next-at) / speed)
		add(next, playback)
		at = next
	}
	source := m.target
	for _, bar := range m.bars[:m.count] {
		if bar.inSplit > 0 {
			add(source+bar.outSplit, playback+bar.inSplit)
		}
		source += bar.out
		playback += bar.in
		add(source, playback)
	}
	return warp
}

func tempoFrameTime(frame int64, rate int) time.Duration {
	return time.Duration(float64(frame) * float64(time.Second) / float64(rate))
}

func tempoTimeFrame(at time.Duration, rate int) int64 {
	return int64(math.Round(at.Seconds() * float64(rate)))
}

func (t *automixTempo) mapTime(at time.Duration, toPlayback bool) time.Duration {
	if t == nil || at <= t.points[0].source {
		return at
	}
	from, to := t.points[0].source, t.points[0].playback
	for _, point := range t.points[1:] {
		nextFrom, nextTo := point.source, point.playback
		if !toPlayback {
			nextFrom, nextTo = nextTo, nextFrom
		}
		if at < nextFrom {
			return to + time.Duration(float64(at-from)*float64(nextTo-to)/float64(nextFrom-from))
		}
		from, to = nextFrom, nextTo
	}
	return to + at - from
}

const rubberBandBlock = 512

type rubberBandAPI struct {
	open  func(uint32) uintptr
	close func(uintptr)
	push  func(uintptr, *float32, int32, float64, int32)
	pull  func(uintptr, *float32, int32) int32
}

type automixStretch struct {
	api    *rubberBandAPI
	state  uintptr
	tempo  *automixTempo
	rate   int
	output int64
	endErr error
	seam   []float32
	input  [rubberBandBlock * 2]float32
}

func (s *automixStretch) read(w *watched, dst []float32) (int, error) {
	frames := len(dst) / 2
	if length := w.Len(); length > 0 {
		end := s.tempo.mapTime(tempoFrameTime(length, s.rate), true)
		frames = min(frames, int(tempoTimeFrame(end, s.rate)-s.output))
	}
	if frames <= 0 {
		return 0, io.EOF
	}
	for {
		n := s.api.pull(s.state, &dst[0], int32(frames))
		if n > 0 {
			// At unity, blend in the first output samples as the analysis window settles.
			first := int(s.output - s.tempo.first)
			for i := range min(int(n), len(s.seam)/2-first) {
				wet := float32(automixRampProgress(int64(first+i), 0, int64(cap(s.seam)/2)))
				for ch := range 2 {
					dry := s.seam[2*(first+i)+ch]
					dst[2*i+ch] = dry + wet*(dst[2*i+ch]-dry)
				}
			}
			if first+int(n) >= cap(s.seam)/2 {
				s.seam = nil
			}
			s.output += int64(n)
			return 2 * int(n), nil
		}
		if s.endErr != nil {
			return 0, s.endErr
		}
		if n < 0 {
			return 0, io.EOF
		}
		from := tempoFrameTime(w.frame, s.rate)
		count, err := w.readPCM(s.input[:])
		s.seam = append(s.seam, s.input[:min(count, cap(s.seam)-len(s.seam))]...)
		to := tempoFrameTime(w.frame, s.rate)
		ratio := 1.0
		if to > from {
			ratio = float64(s.tempo.mapTime(to, true)-s.tempo.mapTime(from, true)) / float64(to-from)
		}
		var final int32
		if err != nil || count == 0 {
			s.endErr = err
			if s.endErr == nil {
				s.endErr = io.EOF
			}
			final = 1
		}
		s.api.push(s.state, &s.input[0], int32(count/2), ratio, final)
	}
}
