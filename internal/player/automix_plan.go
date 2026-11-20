package player

import (
	"math"
	"time"
)

func automixDownbeats(t *beatTimeline) []int {
	var downbeats []int
	for i, beat := range t.events {
		if beat.bar {
			downbeats = append(downbeats, i)
		}
	}
	return downbeats
}

// matchBars ranks complete downbeat-to-downbeat overlaps, retaining pyCrossfade's measured stretch for each matched group.
func matchBars(out, in *beatTimeline, end, position time.Duration) automixPlan {
	if out == nil || in == nil || out.end+100*time.Millisecond < end {
		return automixPlan{}
	}
	from, to := automixDownbeats(out), automixDownbeats(in)
	outPhrases, inPhrases := out.phraseScores(from), in.phraseScores(to)
	best, bestScore, fromBar := automixPlan{}, math.Inf(-1), 0
	for _, meter := range [...][2]int{{1, 1}, {1, 2}, {2, 1}} {
		a, b := meter[0], meter[1]
		maxGroups := min(automixBars/max(a, b), (len(from)-1)/a, (len(to)-1)/b)
		for count := maxGroups; count > 0; count-- {
			for i := len(from) - a*count - 1; i >= 0; i-- {
				target := out.events[from[i]].at
				if target <= position+automixLead+automixAhead+80*time.Millisecond {
					break
				}
				outEnd := out.events[from[i+a*count]].at
				tail := end - outEnd
				if tail < 0 || tail > 6*time.Second {
					continue
				}
				outKey := out.keyBetween(target, outEnd)
				for j := 0; j+b*count < len(to); j++ {
					cue := in.events[to[j]].at
					if cue > 6*time.Second {
						break
					}
					inEnd := in.events[to[j+b*count]].at
					key := harmonicMatch(outKey, in.keyBetween(cue, inEnd))
					if key < 0 && count*max(a, b) > 2 {
						continue
					}
					plan := automixPlan{cue: cue, target: target, overlap: inEnd - cue, inBars: b * count}
					var rhythm float64
					for k := range count {
						old := out.events[from[i+a*k] : from[i+a*(k+1)]+1]
						new := in.events[to[j+b*k] : to[j+b*(k+1)]+1]
						bar, fit, ok := fitAutomixBar(out, in, old, new, a, b)
						if !ok {
							break
						}
						if k > 0 {
							jump := math.Abs(math.Log(bar.speed / plan.bars[k-1].lastSpeed()))
							if jump > math.Log(1.35) {
								break
							}
							fit -= jump
						}
						plan.bars[k], plan.count = bar, k+1
						plan.beats += bar.inBeats
						rhythm += fit
					}
					if plan.count != count {
						continue
					}
					phrase := .55*(outPhrases[i]+inPhrases[j]) + 1.2*(outPhrases[i+a*count]+inPhrases[j+b*count])
					score := .10*float64(count) + .45*float64(key) + phrase + rhythm/float64(count) - .055*(cue+tail).Seconds()
					score -= .48 * automixVocalClash(out, in, &plan)
					if score > bestScore {
						best, bestScore, fromBar = plan, score, i
					}
				}
			}
		}
	}
	for _, index := range from[max(0, fromBar-automixBars):fromBar] {
		at := out.events[index].at
		if at >= position+automixAhead && at < best.target-automixLead {
			best.ramp = append(best.ramp, at)
		}
	}
	return best
}

func reliableAutomixGrid(beats []beatEvent) bool {
	count := len(beats) - 1
	if count < 2 || count > 16 {
		return false
	}
	mean := (beats[count].at - beats[0].at) / time.Duration(count)
	if mean < 150*time.Millisecond || mean > 1500*time.Millisecond {
		return false
	}
	for i := 1; i <= count; i++ {
		gap := beats[i].at - beats[i-1].at
		if gap < max(125*time.Millisecond, mean*55/100) || gap > mean*160/100 {
			return false
		}
	}
	return true
}

func fitAutomixBar(out, in *beatTimeline, old, new []beatEvent, outStride, inStride int) (automixBar, float64, bool) {
	a, b := len(old)-1, len(new)-1
	if a*inStride != b*outStride || !reliableAutomixGrid(old) || !reliableAutomixGrid(new) {
		return automixBar{}, 0, false
	}
	from, to := old[0].at, new[0].at
	outLen, inLen := old[a].at-from, new[b].at-to
	speed := float64(outLen) / float64(inLen)
	if inLen < 750*time.Millisecond || inLen > 6*time.Second || speed < .65 || speed > 1.55 ||
		outStride != inStride && (speed < .8 || speed > 1.25) {
		return automixBar{}, 0, false
	}
	pulses := a / outStride
	if pulses < 2 || a%outStride != 0 || b%inStride != 0 {
		return automixBar{}, 0, false
	}
	bar := automixBar{out: outLen, in: inLen, speed: speed, beats: a, inBeats: b}
	if outStride != inStride {
		fast := new
		if outStride == 2 {
			fast = old
		}
		// The extra downbeat in a doubled grid must land on a real beat in the other track.
		mid := 0
		for i := 1; i < len(fast)-1; i++ {
			if fast[i].bar {
				mid = i
				break
			}
		}
		if mid == 0 || mid%2 != 0 {
			return automixBar{}, 0, false
		}
		bar.outSplit, bar.inSplit = old[mid/2*outStride].at-from, new[mid/2*inStride].at-to
		bar.speed = float64(bar.outSplit) / float64(bar.inSplit)
		bar.nextSpeed = float64(outLen-bar.outSplit) / float64(inLen-bar.inSplit)
		if min(bar.speed, bar.nextSpeed) < .8 || max(bar.speed, bar.nextSpeed) > 1.25 || math.Abs(math.Log(bar.speed/bar.nextSpeed)) > math.Log(1.25) {
			return automixBar{}, 0, false
		}
	}
	// Reject a match when the beats inside its measured stretches still disagree.
	limit := min(95*time.Millisecond, max(45*time.Millisecond, inLen/time.Duration(5*pulses)))
	var timing, sumOut, sumIn float64
	var accentsOut, accentsIn [16]float64
	for i := range pulses {
		a, b := old[i*outStride].at, new[i*inStride].at
		offset, stretch := bar.stretch(b - to)
		drift := time.Duration(float64((a - from - offset).Abs()) / stretch)
		if drift > limit {
			return automixBar{}, 0, false
		}
		timing += float64(drift) / float64(limit)
		accentsOut[i], accentsIn[i] = out.accent(a), in.accent(b)
		sumOut += accentsOut[i]
		sumIn += accentsIn[i]
	}
	var accents float64
	if sumOut > 1e-4 && sumIn > 1e-4 {
		accents = 1
		for i := range pulses {
			accents -= .5 * math.Abs(accentsOut[i]/sumOut-accentsIn[i]/sumIn)
		}
	}
	fit := .4*accents - .75*timing/float64(pulses) - .40*math.Abs(math.Log(speed))
	return bar, fit, true
}

func automixVocalClash(out, in *beatTimeline, plan *automixPlan) float64 {
	if len(out.stems) == 0 || len(in.stems) == 0 {
		return 0
	}
	var clash float64
	for at := automixFeatureStep / 2; at < plan.overlap; at += automixFeatureStep {
		src, _ := plan.alignment(at)
		progress := float64(at) / float64(plan.overlap)
		clash += min(out.vocalAt(src), in.vocalAt(plan.cue+at)) * 4 * progress * (1 - progress)
	}
	return clash * automixFeatureStep.Seconds()
}
