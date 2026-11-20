package player

import (
	"math"
	"time"

	"github.com/egoist/mygo/audio"
	"mygo-music/internal/music"
)

const automixBars = 2

type automixBar struct {
	out, in time.Duration
	speed   float64
	beats   int
}

type automixPlan struct {
	cue, target, ramp, overlap time.Duration
	bars                       [automixBars]automixBar
	count                      int
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
	Bars                             int
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
	info.Active, info.Queued, info.Bars = p.AutomixActive(), true, plan.count
	info.Start, info.Cue, info.Overlap = plan.target, plan.cue, plan.overlap
	if plan.count > 0 {
		bar := plan.bars[0]
		info.FromBPM = 60 * float64(bar.beats) / bar.out.Seconds()
		info.ToBPM = 60 * float64(bar.beats) / bar.in.Seconds()
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
	s, ok := p.stream.(*demucsStream)
	if !ok || s.engine == nil || s.engine.mode == DemucsCPU || s.failed.Load() || p.out == nil ||
		p.Automix == nil || !p.Automix() || p.Dolby != nil && p.Dolby().Spatial {
		return 0, false
	}
	if device := s.engine.device.Load(); device != nil && *device != DemucsCUDA {
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
			prep.watch.automix.Store(nil)
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

// matchBars follows pyCrossfade: stretch each outgoing bar to the measured length of its incoming counterpart.
func matchBars(out, in *beatTimeline, end, position time.Duration) automixPlan {
	if out == nil || in == nil || out.end+100*time.Millisecond < end {
		return automixPlan{}
	}
	downbeats := func(t *beatTimeline) []int {
		var bars []int
		for i, e := range t.events {
			if e.bar {
				bars = append(bars, i)
			}
		}
		return bars
	}
	from, to := downbeats(out), downbeats(in)
	for count := automixBars; count > 0; count-- {
		for i := len(from) - count - 1; i >= 0; i-- {
			target := out.events[from[i]].at
			if target <= position+automixLead+automixAhead {
				break
			}
			if tail := end - out.events[from[i+count]].at; tail < 0 || tail > 3*time.Second {
				continue
			}
			for j := 0; j+count < len(to); j++ {
				cue := in.events[to[j]].at
				if cue > 3*time.Second {
					break
				}
				ramp := max(position+automixAhead, out.events[from[max(0, i-2)]].at)
				if target-ramp <= automixLead+300*time.Millisecond {
					continue
				}
				plan := automixPlan{cue: cue, target: target, ramp: ramp}
				for k := range count {
					outLen := out.events[from[i+k+1]].at - out.events[from[i+k]].at
					inLen := in.events[to[j+k+1]].at - in.events[to[j+k]].at
					beats := from[i+k+1] - from[i+k]
					if beats < 2 || beats != to[j+k+1]-to[j+k] || inLen < 750*time.Millisecond || inLen > 4*time.Second {
						break
					}
					speed := float64(outLen) / float64(inLen)
					if speed < .85 || speed > 1.18 {
						break
					}
					plan.bars[k] = automixBar{out: outLen, in: inLen, speed: speed, beats: beats}
					plan.overlap += inLen
					plan.count++
				}
				if plan.count == count {
					return plan
				}
			}
		}
	}
	return automixPlan{}
}

func (m *automixPlan) outEnd() time.Duration {
	end := m.target
	for _, bar := range m.bars[:m.count] {
		end += bar.out
	}
	return end
}

func (m *automixPlan) approach(at time.Duration) float64 {
	progress := min(1, max(0, float64(at-m.ramp)/float64(m.target-automixLead-m.ramp)))
	return 1 + (m.bars[0].speed-1)*progress
}

// alignment maps elapsed incoming audio to outgoing source time and the speed of that bar.
func (m *automixPlan) alignment(elapsed time.Duration) (time.Duration, float64) {
	at := m.target
	for _, bar := range m.bars[:m.count] {
		if elapsed < bar.in {
			return at + time.Duration(float64(elapsed)*bar.speed), bar.speed
		}
		elapsed -= bar.in
		at += bar.out
	}
	return at, m.bars[m.count-1].speed
}

type automixEQ struct {
	start, end int64
	incoming   bool
}

type automixBands struct {
	envelope        *automixEQ
	low, upper      [2]float32
	lowStep, hiStep float32
}

func (w *watched) setAutomixEQ(start, end time.Duration, incoming bool) {
	rate := float64(w.SampleRate())
	w.automix.Store(&automixEQ{
		start: int64(math.Round(start.Seconds() * rate)),
		end:   int64(math.Round(end.Seconds() * rate)), incoming: incoming,
	})
}

func (b *automixBands) apply(pcm []float32, frame int64, rate int, eq *automixEQ) {
	if eq == nil || frame >= eq.end || frame+int64(len(pcm)/2) <= eq.start {
		return
	}
	if b.envelope != eq {
		*b = automixBands{envelope: eq,
			lowStep: float32(1 - math.Exp(-2*math.Pi*120/float64(rate))),
			hiStep:  float32(1 - math.Exp(-2*math.Pi*6500/float64(rate))),
		}
	}
	// The complementary bands sum to the unfiltered signal. Cross the bass
	// controls faster than the treble and dip both mids around the crossover.
	for i := int(2 * max(0, eq.start-frame)); i+1 < len(pcm); i += 2 {
		at := frame + int64(i/2)
		if at >= eq.end {
			break
		}
		t := float32(at-eq.start) / float32(eq.end-eq.start)
		cut := t
		if eq.incoming {
			cut = 1 - t
		}
		bass, treble := 0.9*min(1, 2*cut), 0.7*cut
		mid := 0.3 * 4 * t * (1 - t)
		for ch := range 2 {
			sample := pcm[i+ch]
			b.low[ch] += b.lowStep * (sample - b.low[ch])
			b.upper[ch] += b.hiStep * (sample - b.upper[ch])
			pcm[i+ch] = sample - bass*b.low[ch] - mid*(b.upper[ch]-b.low[ch]) - treble*(sample-b.upper[ch])
		}
	}
}
