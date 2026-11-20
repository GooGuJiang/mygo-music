package player

import (
	"math"
	"time"

	"github.com/egoist/mygo/audio"
	"mygo-music/internal/music"
)

const automixBars = 8 // pyCrossfade's default overlap and tempo ramp in bars

type automixBar struct {
	out, in           time.Duration
	outSplit, inSplit time.Duration
	speed, nextSpeed  float64
	beats, inBeats    int
}

type automixPlan struct {
	cue, target, overlap time.Duration
	ramp                 []time.Duration
	bars                 [automixBars]automixBar
	count, inBars, beats int
	stems                automixStemTiming
}

type automixPreparation struct {
	plan  automixPlan
	next  *nextRhythm
	out   *audio.Player
	watch *watched
}

// AutomixInfo describes the queued or playing transition in source time.
type AutomixInfo struct {
	Active, Queued                   bool
	Preloading, Ready, PreloadFailed bool
	Progress                         float64
	Next                             string
	Bars, Beats                      int
	FromBPM, ToBPM                   float64
	Start, Cue                       time.Duration
	Overlap                          time.Duration
	Estimated                        bool
}

func (p *Player) AutomixInfo() AutomixInfo {
	var info AutomixInfo
	plan := p.automixMix
	if plan != nil && p.track != nil {
		info.Next = p.track.Name
	} else {
		if next, ok := p.upNext(); ok {
			info.Next = next.Name
			if a := p.ahead; a != nil && a.is(next, p.playbackQuality()) {
				info.Ready, info.PreloadFailed, info.Progress = a.transfer()
				info.Preloading = !info.Ready && !info.PreloadFailed
			}
		}
		if p.automixPrep != nil {
			plan = &p.automixPrep.plan
		}
	}
	if plan == nil {
		if point, ok := p.estimatedAutomixPoint(); ok {
			info.Start, info.Estimated = point, true
		}
		return info
	}
	info.Active, info.Queued, info.Bars, info.Beats = p.AutomixActive(), true, plan.inBars, plan.beats
	info.Start, info.Cue, info.Overlap = plan.target, plan.cue, plan.overlap
	if plan.count > 0 {
		bar := plan.bars[0]
		info.FromBPM = 60 * float64(bar.beats) / bar.out.Seconds()
		info.ToBPM = 60 * float64(bar.inBeats) / bar.in.Seconds()
	}
	return info
}

// AutomixPoint reports the transition position on the currently displayed track.
func (p *Player) AutomixPoint() (point time.Duration, estimated, ok bool) {
	if p.automixMix != nil {
		return p.automixMix.cue, false, p.out != nil
	}
	if p.automixPrep != nil {
		return p.automixPrep.plan.target, false, true
	}
	point, ok = p.estimatedAutomixPoint()
	return point, ok, ok
}

func (p *Player) estimatedAutomixPoint() (time.Duration, bool) {
	if p.out == nil || p.stream == nil || p.track != nil && p.track.ShowID != "" ||
		p.Automix == nil || !p.Automix() || p.Dolby != nil && p.Dolby().Spatial {
		return 0, false
	}
	t, ok := p.upNext()
	if !ok || t.ShowID != "" || p.out.Duration() < 2*automixOverlap {
		return 0, false
	}
	if src := p.sourceOf(&t); src == nil || src.Info().Playback == music.PlaybackRemote {
		return 0, false
	}
	point := p.out.Duration() - automixOverlap
	return point, p.out.Position() <= point
}

func (p *Player) cancelAutomixPrep() {
	if prep := p.automixPrep; prep != nil {
		p.automixPrep = nil
		prep.out.SetSpeed(1).SetPreservesPitch(false)
		if prep.watch != nil {
			prep.watch.setAutomix(nil, false)
		}
	}
}

func automixMixPosition(out *audio.Player) time.Duration {
	position := out.Position()
	if audio.Err() == nil {
		position += time.Duration(float64(automixAhead) * out.Speed())
	}
	return position
}

func (m *automixPlan) outEnd() time.Duration {
	end := m.target
	for _, bar := range m.bars[:m.count] {
		end += bar.out
	}
	return end
}

func (m *automixPlan) approach(at time.Duration) (speed float64, next time.Duration) {
	step := 0
	next = m.target
	for i, downbeat := range m.ramp {
		if at < downbeat {
			next = downbeat
			break
		}
		step = i + 1
	}
	progress := float64(max(0, step-1)) / float64(max(1, len(m.ramp)))
	return 1 + (m.bars[0].speed-1)*progress, next
}

// alignment maps elapsed incoming audio to outgoing source time and the speed of that bar.
func (m *automixPlan) alignment(elapsed time.Duration) (time.Duration, float64) {
	at := m.target
	for _, bar := range m.bars[:m.count] {
		if elapsed < bar.in {
			offset, speed := bar.stretch(elapsed)
			return at + offset, speed
		}
		elapsed -= bar.in
		at += bar.out
	}
	return at, m.bars[m.count-1].lastSpeed()
}

func (bar automixBar) stretch(elapsed time.Duration) (time.Duration, float64) {
	if bar.inSplit > 0 && elapsed >= bar.inSplit {
		return bar.outSplit + time.Duration(float64(elapsed-bar.inSplit)*bar.nextSpeed), bar.nextSpeed
	}
	return time.Duration(float64(elapsed) * bar.speed), bar.speed
}

func (bar automixBar) lastSpeed() float64 {
	if bar.nextSpeed > 0 {
		return bar.nextSpeed
	}
	return bar.speed
}

type automixEQ struct {
	start, end int64
	incoming   bool
	steps      [20]int64
}

type automixBands struct {
	envelope *automixEQ
	step     int
	filters  [3]stereoBiquad
}

func newAutomixEQ(plan *automixPlan, rate int, incoming bool) *automixEQ {
	frameAt := func(at time.Duration) int64 { return int64(math.Round(at.Seconds() * float64(rate))) }
	eq := &automixEQ{incoming: incoming}
	for i := range eq.steps {
		elapsed := time.Duration(i) * plan.overlap / time.Duration(len(eq.steps))
		at := plan.cue + elapsed
		if !incoming {
			at, _ = plan.alignment(elapsed)
		}
		eq.steps[i] = frameAt(at)
	}
	eq.start = eq.steps[0]
	if incoming {
		eq.end = frameAt(plan.cue + plan.overlap)
	} else {
		eq.end = frameAt(plan.outEnd())
	}
	return eq
}

func (b *automixBands) apply(pcm []float32, frame int64, rate int, eq *automixEQ) {
	if eq == nil || frame >= eq.end || frame+int64(len(pcm)/2) <= eq.start {
		return
	}
	if b.envelope != eq {
		*b = automixBands{envelope: eq, step: -1}
	}
	// Follow pyCrossfade's stepped shelves and middle dip, preserving filter state between steps.
	edge := int64(max(1, rate/200))
	for i, end := int(2*max(0, eq.start-frame)), min(len(pcm), int(2*(eq.end-frame))); i+1 < end; i += 2 {
		at := frame + int64(i/2)
		step := b.step
		for step+1 < len(eq.steps) && at >= eq.steps[step+1] {
			step++
		}
		if step != b.step {
			b.step = step
			t := float64(step) / float64(len(eq.steps)-1)
			cut := .1 + .9*t
			if eq.incoming {
				cut = .9 * (1 - t)
			}
			const q = math.Sqrt2 / 2
			b.filters[0].shelf(rate, 70, q, -26*cut, false)
			b.filters[1].shelf(rate, 13000, q, -26*cut, true)
			b.filters[2].peak(rate, 1000, q, -6*math.Sin(math.Pi*t))
		}
		wet := float64(min(edge, at-eq.start, eq.end-at)) / float64(edge)
		for ch := range 2 {
			x := float64(pcm[i+ch])
			y := x
			for k := range b.filters {
				y = b.filters[k].sample(ch, y)
			}
			pcm[i+ch] = float32(x + wet*(y-x))
		}
	}
}
