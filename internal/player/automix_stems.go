package player

import (
	"math"
	"time"
)

type automixStemTiming struct {
	duck, drums, vocal time.Duration
}

func planAutomixStems(in *beatTimeline, plan *automixPlan) automixStemTiming {
	bar := plan.bars[0]
	beat := bar.in / time.Duration(bar.inBeats)
	duck := min(750*time.Millisecond, bar.out/time.Duration(bar.beats))
	vocal := 4 * plan.overlap / 5
	start := plan.cue + max(2*beat, plan.overlap/3)
	if entrance, ok := in.vocalEntrance(start, plan.cue+plan.overlap); ok {
		vocal = entrance - plan.cue
	}
	// Finish trading the drums on the last beat with room for the new singer to enter.
	deadline := vocal - min(beat/2, 250*time.Millisecond)
	drums := min(vocal/2, deadline)
	for _, event := range in.events {
		elapsed := event.at - plan.cue
		if elapsed > deadline {
			break
		}
		if elapsed > 0 {
			drums = elapsed
		}
	}
	return automixStemTiming{duck: duck, drums: drums, vocal: vocal}
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
	start, duck, drums, vocal int64
	incoming                  bool
}

func newAutomixStemEnvelope(plan *automixPlan, rate int, incoming bool) *automixStemEnvelope {
	frame := func(at time.Duration) int64 { return int64(math.Round(at.Seconds() * float64(rate))) }
	timing := plan.stems
	if incoming {
		return &automixStemEnvelope{start: frame(plan.cue), drums: frame(plan.cue + timing.drums), vocal: frame(plan.cue + timing.vocal), incoming: true}
	}
	drums, _ := plan.alignment(timing.drums)
	return &automixStemEnvelope{start: frame(plan.target), duck: frame(max(0, plan.target-timing.duck)), drums: frame(drums)}
}

func (env *automixStemEnvelope) gains(frame int64, mix demucsGains) demucsGains {
	if env == nil {
		return mix
	}
	ramp := func(start, end int64) float32 {
		t := float32(min(max(0, frame-start), max(1, end-start))) / float32(max(1, end-start))
		return t * t * (3 - 2*t)
	}
	var voice, drums float32
	if env.incoming {
		if frame < env.start {
			return mix
		}
		voice = .25 + .75*ramp(env.drums, env.vocal)
		drums = .55 + .45*ramp(env.start, env.drums)
	} else {
		voice = 1 - .75*ramp(env.duck, env.start)
		drums = 1 - .45*ramp(env.start, env.drums)
	}
	if voice != 1 {
		mix[demucsVocals] = (1+mix[demucsVocals])*voice - 1
	}
	if drums != 1 {
		mix[demucsDrums] = (1+mix[demucsDrums])*drums - 1
	}
	return mix
}
