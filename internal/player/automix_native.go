package player

import (
	"math"
	"time"
)

const (
	automixNativeLookback = 16 * time.Second
	automixNativeIntro    = 8 * time.Second
)

type automixNativeCut struct {
	loop                                 automixLoopCut
	outBeat, inBeat                      time.Duration // the outgoing and incoming takeover positions
	lead, tail                           time.Duration
	oldVoiceEnd, newVoiceStart, bassFade time.Duration
	lowpass                              automixLowpassTail
}

type automixNativeMix struct {
	cut     automixNativeCut
	out, in *automixLoopAudio
}

type automixLowpassTail struct {
	at, bassEnd time.Duration
	bassGain    float32
}

func planAutomixLowpass(in *beatTimeline, cue, pivot, tail time.Duration, old, carry automixStem) automixLowpassTail {
	filter := automixLowpassTail{at: pivot}
	reference := .65 * old.power[demucsBass]
	available := carry.power[demucsBass]
	if reference < .0002 || available < .00015 {
		return filter
	}
	low := func(at time.Duration) float64 {
		stem := in.stemAt(at)
		power := stem.power[demucsBass] + .12*stem.power[demucsDrums]
		i := int((at - in.start) / automixFeatureStep)
		if at >= in.start && i < len(in.energy) {
			power = max(power, .7*in.energy[i].low)
		}
		return power
	}
	// Ignore the first transient when estimating whether the new bass can sustain the handoff.
	entry := (low(cue+automixFeatureStep) + low(cue+2*automixFeatureStep) + low(cue+3*automixFeatureStep)) / 3
	if entry >= reference {
		return filter
	}
	limit := min(250*time.Millisecond, tail-80*time.Millisecond)
	for offset := 100 * time.Millisecond; offset < limit; offset += automixFeatureStep {
		if low(cue+offset) >= reference {
			limit = offset
			break
		}
	}
	filter.bassEnd = pivot + limit
	filter.bassGain = float32(min(.78, math.Sqrt((reference-entry)/available)))
	return filter
}

func planAutomixNative(out, in *beatTimeline, end time.Duration) automixNativeCut {
	from, to := automixDownbeats(out), automixDownbeats(in)
	oldPhrases, newPhrases := out.phraseScores(from), in.phraseScores(to)
	loops := automixLoopCandidates(out, end)
	var best automixNativeCut
	bestScore := math.Inf(-1)
	for j := 0; j+1 < len(to); j++ {
		index := to[j]
		bar := in.events[index : to[j+1]+1]
		cue := bar[0].at
		if cue > 6*time.Second {
			break
		}
		if index == 0 || !reliableAutomixGrid(bar) {
			continue
		}
		beat := (bar[len(bar)-1].at - cue) / time.Duration(len(bar)-1)
		tail := min(600*time.Millisecond, max(360*time.Millisecond, beat))
		entry, ok := automixLoopFeatures(in, cue, cue+tail)
		if !ok || entry.power[demucsDrums] < .00015 {
			continue
		}
		for count := 1; count <= min(2, index); count++ {
			start := in.events[index-count].at
			lead := cue - start
			if lead < 260*time.Millisecond || lead > 1200*time.Millisecond {
				continue
			}
			preview, ok := automixLoopFeatures(in, start, cue)
			voice, pickupOK := automixNativePickup(in, start, cue)
			if !ok || !pickupOK || preview.power[demucsOther] < .00015 {
				continue
			}
			consider := func(loop automixLoopCut, source, pivot, period time.Duration, phrase, cost float64) {
				if left := end - pivot - tail; left < 100*time.Millisecond || left > automixOverlap {
					return
				}
				if loop.bars != 0 && loop.end-loop.start < lead {
					return
				}
				old, oldOK := automixLoopFeatures(out, source-lead, source)
				carry, carryOK := automixLoopFeatures(out, source, source+tail)
				lastVoice, voiceOK := automixNativeVoiceEnd(out, source-max(lead, 1400*time.Millisecond), end)
				if !oldOK || !carryOK || !voiceOK || lastVoice > source-80*time.Millisecond {
					return
				}
				if loop.bars == 0 && (lastVoice > pivot+(voice-cue)-65*time.Millisecond || cost > 0 && lastVoice == 0) {
					return
				}
				key := harmonicMatch(estimateKey(old.chroma), estimateKey(preview.chroma))
				clash := backingClash(old, preview, key)
				carryClash := backingClash(carry, entry, key)
				if old.power[demucsOther] > .0006 && (key < 0 && clash > .38 || clash > .65) ||
					carry.power[demucsOther] > .0007 && carryClash > .58 {
					return
				}
				// Compare the local levels at the change of pulse, including the low end on its own.
				outPower := old.power[demucsDrums] + old.power[demucsBass] + old.power[demucsOther]
				inPower := entry.power[demucsDrums] + entry.power[demucsBass] + entry.power[demucsOther]
				jump := math.Abs(math.Log((outPower + .0007) / (inPower + .0007)))
				bassJump := math.Abs(math.Log((old.power[demucsBass] + .0007) / (entry.power[demucsBass] + .0007)))
				if jump > 2.8 {
					return
				}
				score := .7*newPhrases[j] + .35*phrase + .12*lead.Seconds() - .10*((end-pivot).Seconds()+start.Seconds()) -
					.60*clash - .36*carryClash - .21*jump - .09*bassJump - cost
				if score > bestScore {
					bassFade := time.Duration(float64(180*time.Millisecond) / (1 + entry.power[demucsBass]/(old.power[demucsBass]+.001)))
					best = automixNativeCut{loop: loop, outBeat: pivot, inBeat: cue, lead: lead, tail: tail,
						oldVoiceEnd: lastVoice, newVoiceStart: voice, bassFade: max(55*time.Millisecond, bassFade)}
					if lead <= 750*time.Millisecond || !automixLoopCanStretch(period, beat) {
						best.lowpass = planAutomixLowpass(in, cue, pivot, tail, old, carry)
					}
					bestScore = score
				}
			}
			for i := 1; i < len(from); i++ {
				old := out.events[from[i-1] : from[i]+1]
				pivot := old[len(old)-1].at
				if end-pivot > automixOverlap+tail || !reliableAutomixGrid(old) {
					continue
				}
				period := (pivot - old[0].at) / time.Duration(len(old)-1)
				body, bodyOK := automixLoopFeatures(out, old[0].at, pivot)
				gap, gapOK := automixLoopFeatures(out, pivot-min(220*time.Millisecond, period/2), pivot+min(200*time.Millisecond, tail))
				if bodyOK && gapOK {
					cost := 0.0
					if gap.power[demucsDrums] > max(.00065, .42*body.power[demucsDrums]) {
						// A finished vocal line can make room even if its drum groove is still playing.
						cost = .18
					}
					consider(automixLoopCut{}, pivot, pivot, period, oldPhrases[i], cost)
				}
			}
			for _, loop := range loops {
				before, beforeOK := automixLoopFeatures(out, loop.end-loop.beat, loop.end)
				after, afterOK := automixLoopFeatures(out, loop.start-automixLoopSeam, loop.start+loop.beat)
				drumJump := math.Abs(math.Log((before.power[demucsDrums] + .00015) / (after.power[demucsDrums] + .00015)))
				if beforeOK && afterOK && after.power[demucsDrums] >= .00015 && drumJump <= 1.6 {
					consider(loop, loop.end, loop.target(), loop.beat, 0, .13+.13*drumJump+.13*backingClash(before, after, 0))
				}
			}
		}
	}
	return best
}

func automixNativeVoiceEnd(t *beatTimeline, from, end time.Duration) (time.Duration, bool) {
	var last time.Duration
	for at := from; at < min(end, t.start+time.Duration(len(t.stems))*automixFeatureStep); at += automixFeatureStep {
		stem := t.stemAt(at)
		if !stem.known {
			return 0, false
		}
		if stem.vocal >= .32 {
			last = at + automixFeatureStep
		}
	}
	return last, true
}

func automixNativePickup(t *beatTimeline, from, cue time.Duration) (time.Duration, bool) {
	for at := from; at < cue; at += automixFeatureStep {
		if t.stemAt(at).vocal >= .38 {
			if at == from && from > 0 && t.stemAt(max(0, from-automixFeatureStep)).vocal >= .32 {
				return 0, false // the preview would begin in the middle of a line
			}
			return max(from, at-automixFeatureStep), true
		}
	}
	return cue - 25*time.Millisecond, true
}

func renderAutomixNative(c automixNativeCut, old, next automixStemSource) *automixNativeMix {
	outStart := c.outBeat - c.lead - 25*time.Millisecond
	if c.loop.bars != 0 {
		outStart = min(outStart, c.loop.end-75*time.Millisecond)
	}
	inStart := c.inBeat - c.lead
	backingGain := func(elapsed time.Duration) float32 {
		return .40*nativeRamp(elapsed, -c.lead, -c.lead/4) + .60*nativeRamp(elapsed, -c.lead/4, c.tail-35*time.Millisecond)
	}
	out := old.render(outStart, c.outBeat+c.tail, c.loop, false, c.lowpass, func(at time.Duration) [demucsStemCount]float32 {
		voiceEnd := c.outBeat - 15*time.Millisecond
		if c.loop.bars != 0 {
			voiceEnd = c.loop.end - automixLoopSeam
		}
		return [demucsStemCount]float32{
			1 - nativeRamp(at, max(c.oldVoiceEnd, voiceEnd-65*time.Millisecond), voiceEnd),
			1 - nativeRamp(at, c.outBeat-85*time.Millisecond, c.outBeat),
			1 - nativeRamp(at, c.outBeat-c.bassFade, c.outBeat),
			1 - backingGain(at-c.outBeat),
		}
	})
	if out == nil {
		return nil
	}
	in := next.render(inStart, c.inBeat+c.tail, automixLoopCut{}, true, automixLowpassTail{}, func(at time.Duration) [demucsStemCount]float32 {
		return [demucsStemCount]float32{
			nativeRamp(at, c.newVoiceStart-20*time.Millisecond, c.newVoiceStart+15*time.Millisecond),
			nativeRamp(at, c.inBeat-4*time.Millisecond, c.inBeat+12*time.Millisecond),
			nativeRamp(at, c.inBeat, c.inBeat+35*time.Millisecond),
			backingGain(at - c.inBeat),
		}
	})
	if in == nil {
		return nil
	}
	return &automixNativeMix{c, out, in}
}

func nativeRamp(at, from, to time.Duration) float32 {
	return float32(automixRampProgress(int64(at), int64(from), int64(to)))
}
