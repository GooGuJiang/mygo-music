package player

import (
	"math"
	"time"
)

type automixStemTiming struct {
	out, in  [demucsStemCount]automixStemRamp
	analyzed bool
}

type automixStemRamp struct {
	start, end time.Duration
	depth      float64
}

type automixStemSample struct {
	at      time.Duration
	out, in automixStem
}

func planAutomixStems(out, in *beatTimeline, plan *automixPlan) automixStemTiming {
	bar := plan.bars[0]
	beat := bar.in / time.Duration(bar.inBeats)
	duck := min(750*time.Millisecond, bar.out/time.Duration(bar.beats))
	vocal := 4 * plan.overlap / 5
	start := plan.cue + max(2*beat, plan.overlap/3)
	if entrance, ok := in.vocalEntrance(start, plan.cue+plan.overlap); ok {
		vocal = entrance - plan.cue
	}
	// Finish trading the drums on the last beat with room for the new singer to enter.
	deadline := max(beat, vocal-min(beat/2, 250*time.Millisecond))
	drums := min(vocal/2, deadline)
	var beats []time.Duration
	for _, event := range in.events {
		elapsed := event.at - plan.cue
		if elapsed >= plan.overlap {
			break
		}
		if elapsed > 0 {
			beats = append(beats, elapsed)
			if elapsed <= deadline {
				drums = elapsed
			}
		}
	}
	planned := automixStemTiming{}
	drumRamp := automixStemRamp{0, drums, .45}
	bassRamp := automixStemRamp{max(0, drums-beat), min(plan.overlap, drums+beat/2), .20}
	otherRamp := automixStemRamp{plan.overlap / 5, 4 * plan.overlap / 5, .12}
	voiceDepth := .75
	voiceOutEnd := time.Duration(0)
	var samples []automixStemSample
	var doubles [demucsStemCount]float64
	var vocalPeak, vocalOverlap, harmony, backing float64
	var drumOut, drumIn, drumCommon float64
	key := harmonicMatch(out.keyBetween(plan.target, plan.outEnd()), in.keyBetween(plan.cue, plan.cue+plan.overlap))
	for at := automixFeatureStep; at < plan.overlap; at += 2 * automixFeatureStep {
		old, _ := plan.alignment(at)
		a, b := out.stemAt(old), in.stemAt(plan.cue+at)
		if !a.known || !b.known {
			continue
		}
		samples = append(samples, automixStemSample{at, a, b})
		dualVocal := min(a.vocal, b.vocal)
		vocalPeak = max(vocalPeak, dualVocal)
		vocalOverlap += dualVocal
		for stem := demucsDrums; stem < demucsStemCount; stem++ {
			doubles[stem] += stemOverlap(a, b, stem)
		}
		drumOut += a.power[demucsDrums]
		drumIn += b.power[demucsDrums]
		drumCommon += math.Sqrt(a.power[demucsDrums] * b.power[demucsDrums])
		weight := stemOverlap(a, b, demucsOther)
		harmony += weight * backingClash(a, b, key)
		backing += weight
	}
	if len(samples) >= 4 && time.Duration(len(samples))*2*automixFeatureStep >= plan.overlap/2 {
		planned.analyzed = true
		for stem := range doubles {
			doubles[stem] /= float64(len(samples))
		}
		harmony /= max(.001, backing)
		drumMismatch := 0.0
		if min(drumOut, drumIn) > .0007*float64(len(samples)) {
			drumMismatch = max(0, 1-drumCommon/math.Sqrt(drumOut*drumIn))
		}
		// A single active 100 ms observation should not duck an otherwise clean vocal handoff.
		voiceDepth = .82 * min(1, 1.35*vocalPeak) * min(1, max(0, (vocalOverlap-1.5)/3))
		if entrance, ok := in.vocalEntrance(start, plan.cue+plan.overlap); ok {
			voiceOutEnd = max(0, entrance-plan.cue-beat/2)
		}
		drumWidth := time.Duration(float64(3*beat) * (1 - .65*drumMismatch))
		drumRamp = chooseAutomixRamp(samples, beats, plan.overlap, deadline, beat, demucsDrums,
			automixStemRamp{max(0, drums-drumWidth), drums, .20 + .22*doubles[demucsDrums] + .32*drumMismatch}, .34+.45*drumMismatch)
		drumCenter := (drumRamp.start + drumRamp.end) / 2
		bassRamp = chooseAutomixRamp(samples, beats, plan.overlap, vocal, beat, demucsBass,
			automixStemRamp{max(0, drumCenter-beat/2), min(plan.overlap, drumCenter+beat/2), .15 + .52*doubles[demucsBass]}, .88)
		otherWidth := time.Duration(float64(min(plan.overlap, 6*beat)) * (1 - .65*harmony))
		otherRamp = chooseAutomixRamp(samples, beats, plan.overlap, plan.overlap, beat, demucsOther,
			automixStemRamp{(plan.overlap - otherWidth) / 2, (plan.overlap + otherWidth) / 2, .12 + .55*harmony*doubles[demucsOther]}, .25+.63*harmony)
	}
	planned.out[demucsVocals] = automixStemRamp{voiceOutEnd - duck, voiceOutEnd, voiceDepth}
	planned.in[demucsVocals] = automixStemRamp{min(vocal-min(80*time.Millisecond, vocal/2), drumRamp.end), vocal, voiceDepth}
	for i, ramp := range [...]automixStemRamp{drumRamp, bassRamp, otherRamp} {
		stem := demucsDrums + i
		planned.out[stem], planned.in[stem] = ramp, ramp
	}
	return planned
}

func stemOverlap(a, b automixStem, stem int) float64 {
	power := min(a.power[stem], b.power[stem])
	return power / (power + .0007)
}

func backingClash(a, b automixStem, key int) float64 {
	var above, below, common float64
	for pitch, strength := range a.chroma {
		above += strength
		below += b.chroma[pitch]
		common += min(strength, b.chroma[pitch])
	}
	if above > 0 && below > 0 {
		return max(0, 1-common)
	}
	if key < 0 {
		return .8
	}
	return .25
}

// Score a small set of beat-centered ramps by level loss and double-stem energy.
func chooseAutomixRamp(samples []automixStemSample, beats []time.Duration, overlap, limit, beat time.Duration, stem int, preferred automixStemRamp, weight float64) automixStemRamp {
	center := (preferred.start + preferred.end) / 2
	best, bestCost := preferred, math.Inf(1)
	width := max(80*time.Millisecond, preferred.end-preferred.start)
	for _, at := range beats {
		if at >= limit || (at-center).Abs() > 2*beat {
			continue
		}
		for _, span := range [...]time.Duration{width, 3 * width / 4} {
			// Clip symmetrically so the two curves still meet on the beat.
			half := min(span/2, at, limit-at)
			if half < 40*time.Millisecond {
				continue
			}
			ramp := automixStemRamp{at - half, at + half, preferred.depth}
			var energy, loss, collision float64
			for _, sample := range samples {
				p := float64(sample.at) / float64(overlap)
				a, b := (1-p)*sample.out.power[stem], p*sample.in.power[stem]
				shape := automixRampProgress(int64(sample.at), int64(ramp.start), int64(ramp.end))
				g1, g2 := 1-ramp.depth*shape, 1-ramp.depth*(1-shape)
				x, y := a*g1*g1, b*g2*g2
				energy += a + b
				loss += a + b - x - y
				collision += 2 * min(x, y)
			}
			cost := (loss+weight*collision)/(energy+1e-6) + .045*float64((at-center).Abs())/float64(beat)
			if cost < bestCost {
				best, bestCost = ramp, cost
			}
		}
	}
	return best
}

func automixRampProgress(at, start, end int64) float64 {
	t := automixRampPosition(at, start, end)
	return t * t * (3 - 2*t)
}

func automixRampPosition(at, start, end int64) float64 {
	span := max(1, end-start)
	return float64(min(max(0, at-start), span)) / float64(span)
}

// Require known quiet audio before a sustained entrance. Missing analysis can't masquerade as silence.
func (t *beatTimeline) vocalEntrance(start, end time.Duration) (time.Duration, bool) {
	const quiet, singing = 4, 4
	for i := quiet; i+singing <= len(t.stems); i++ {
		at := t.start + time.Duration(i)*automixFeatureStep
		if at > end {
			break
		}
		if at < start {
			continue
		}
		ok := true
		for _, frame := range t.stems[i-quiet : i] {
			ok = ok && frame.known && frame.vocal < .35
		}
		for _, frame := range t.stems[i : i+singing] {
			ok = ok && frame.known && frame.vocal >= .55
		}
		if ok {
			return at, true
		}
	}
	return 0, false
}

type automixStemEnvelope struct {
	start    int64
	incoming bool
	ramps    [demucsStemCount]struct {
		start, middle, end int64
		depth              float32
	}
}

func newAutomixStemEnvelope(plan *automixPlan, rate int, incoming bool) *automixStemEnvelope {
	frame := func(at time.Duration) int64 { return int64(math.Round(at.Seconds() * float64(rate))) }
	mapFrame := func(elapsed time.Duration) int64 {
		if incoming {
			return frame(plan.cue + elapsed)
		}
		if elapsed < 0 {
			return frame(max(0, plan.target+elapsed))
		}
		at, _ := plan.alignment(elapsed)
		return frame(at)
	}
	env := &automixStemEnvelope{start: frame(plan.cue), incoming: incoming}
	for stem := range env.ramps {
		ramp := plan.stems.in[stem]
		if !incoming {
			ramp = plan.stems.out[stem]
		}
		env.ramps[stem].start = mapFrame(ramp.start)
		env.ramps[stem].middle = mapFrame((ramp.start + ramp.end) / 2)
		env.ramps[stem].end = mapFrame(ramp.end)
		env.ramps[stem].depth = float32(ramp.depth)
	}
	return env
}

func (env *automixStemEnvelope) gains(frame int64, mix demucsGains) demucsGains {
	if env == nil || env.incoming && frame < env.start {
		return mix
	}
	for stem, ramp := range env.ramps {
		if ramp.depth == 0 {
			continue
		}
		// Preserve the midpoint beat even when the two halves have different stretch rates.
		position := .5 * automixRampPosition(frame, ramp.start, ramp.middle)
		if frame >= ramp.middle {
			position = .5 + .5*automixRampPosition(frame, ramp.middle, ramp.end)
		}
		cut := float32(position * position * (3 - 2*position))
		if env.incoming {
			cut = 1 - cut
		}
		if cut > 0 {
			mix[stem] = (1+mix[stem])*(1-ramp.depth*cut) - 1
		}
	}
	return mix
}
