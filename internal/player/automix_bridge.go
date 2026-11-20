package player

import (
	"math"
	"time"
)

// Bridge the pulse change on nearby beats at each track's original speed.
func planAutomixBridge(out, in *beatTimeline, end, position time.Duration) automixPlan {
	if out == nil || in == nil || out.end+100*time.Millisecond < end {
		return automixPlan{}
	}
	reliableBeats := func(t *beatTimeline) []beatEvent {
		var beats []beatEvent
		bars := automixDownbeats(t)
		for i := 1; i < len(bars); i++ {
			bar := t.events[bars[i-1] : bars[i]+1]
			if reliableAutomixGrid(bar) {
				if len(beats) > 0 && beats[len(beats)-1].at == bar[0].at {
					bar = bar[1:]
				}
				beats = append(beats, bar...)
			}
		}
		return beats
	}
	oldBeats, nextBeats := reliableBeats(out), reliableBeats(in)
	best, bestScore := automixPlan{}, math.Inf(-1)
	for _, next := range nextBeats {
		pivot := min(next.at, 750*time.Millisecond)
		cue := next.at - pivot
		if cue > 1200*time.Millisecond {
			break
		}
		overlap := pivot + 500*time.Millisecond
		if cue+overlap > in.end {
			continue
		}
		for _, old := range oldBeats {
			target := old.at - pivot
			if target <= position+automixAhead+automixLead+80*time.Millisecond {
				continue
			}
			tail := end - (target + overlap)
			if tail < 0 || tail > 2200*time.Millisecond {
				continue
			}
			score := -.60*cue.Seconds() - .49*tail.Seconds() - 2*out.accent(old.at) - in.accent(cue)
			if next.bar {
				score += .55
			}
			if old.bar {
				score += .18
			}
			if score > bestScore {
				best = automixPlan{bridge: true, target: target, cue: cue, overlap: overlap, pivot: pivot}
				bestScore = score
			}
		}
	}
	if best.bridge {
		// Let the old rhythm give way before the new beat enters, with tonal material carrying the short overlap.
		for _, stem := range [...]int{demucsDrums, demucsBass} {
			best.stems.out[stem] = automixStemRamp{best.pivot - 380*time.Millisecond, best.pivot, .90}
			if best.pivot > 0 {
				best.stems.in[stem] = automixStemRamp{max(0, best.pivot-140*time.Millisecond), best.pivot, .82}
			}
		}
		best.stems.out[demucsVocals] = automixStemRamp{-180 * time.Millisecond, best.pivot + 200*time.Millisecond, .50}
		best.stems.in[demucsVocals] = automixStemRamp{best.pivot, best.overlap, .30}
	}
	return best
}

func (m *automixPlan) bridgeProgress(level float64) float64 {
	center := max(.20, float64(m.pivot)/float64(m.overlap))
	if level <= center {
		return .5 * level / center
	}
	return .5 + .5*(level-center)/(1-center)
}
