package player

import (
	"testing"
	"time"
)

// A natural gap preserves the last line and the incoming pickup, and a clean loop can replace a missing gap.
func TestAutomixNativeRanksGapAndLoop(t *testing.T) {
	const oldBar, newBar = 2640 * time.Millisecond, 1760 * time.Millisecond
	out := automixGrid(oldBar, oldBar, oldBar, oldBar, oldBar, oldBar)
	in := automixGrid(newBar, newBar, newBar, newBar)
	for _, grid := range []*beatTimeline{out, in} {
		grid.stems = make([]automixStem, int(grid.end/automixFeatureStep))
		for i := range grid.stems {
			grid.stems[i] = automixStem{known: true, chroma: [12]float64{0: .5, 4: .25, 7: .25}, power: [demucsStemCount]float64{0, .02, .01, .025}}
		}
	}
	const cross = 5 * oldBar
	for i := range out.stems {
		at := time.Duration(i) * automixFeatureStep
		if at >= cross-1200*time.Millisecond && at < cross-450*time.Millisecond {
			out.stems[i].vocal = .8
		}
		if (at - cross).Abs() <= 300*time.Millisecond {
			out.stems[i].power[demucsDrums] = .0001
		}
	}
	for i := range in.stems {
		at := time.Duration(i) * automixFeatureStep
		if at >= newBar-140*time.Millisecond && at <= newBar+180*time.Millisecond {
			in.stems[i].vocal = .8
		}
	}
	// Later incoming bars are in a different key, leaving only the opening pickup available.
	for i := int(2 * newBar / automixFeatureStep); i < len(in.stems); i++ {
		in.stems[i].chroma = [12]float64{1: .25, 6: .5, 10: .25}
		in.stems[i].power[demucsOther] = 0
	}
	cut := planAutomixNative(out, in, out.end)
	if cut.loop.bars != 0 || cut.outBeat != cross || cut.inBeat != newBar || cut.lead <= 0 ||
		cut.oldVoiceEnd <= cross-550*time.Millisecond || cut.oldVoiceEnd >= cross-300*time.Millisecond || cut.newVoiceStart >= newBar-80*time.Millisecond {
		t.Fatalf("expected the natural gap with a complete outgoing line and incoming pickup: %+v", cut)
	}
	for i := range out.stems {
		out.stems[i].vocal = 0
	}
	if cut := planAutomixNative(out, in, out.end); cut.lead <= 0 || cut.loop.bars != 0 {
		t.Fatalf("preferred repeating over a clean natural gap: %+v", cut)
	}
	for i := range out.stems {
		out.stems[i].power[demucsDrums] = .02
	}
	if cut := planAutomixNative(out, in, out.end); cut.loop.bars < 1 || cut.loop.bars > 2 || cut.inBeat != newBar {
		t.Fatalf("expected a short loop when the original groove has no gap: %+v", cut)
	}
	for i := range out.stems {
		out.stems[i].vocal = .85
	}
	if cut := planAutomixNative(out, in, out.end); cut.lead != 0 {
		t.Fatalf("a native transition would cut the old singer mid-line: %+v", cut)
	}
}

// The prepared PCM plays the incoming accompaniment early, keeps the old singer through the last line, and hands off bass and drums at the downbeat.
func TestAutomixNativeRendersStaggeredEntry(t *testing.T) {
	const rate = 48000
	dry := make([]float32, 2*6*rate)
	split := make([][2 * demucsOther]float32, len(dry)/2)
	for i := range split {
		split[i] = [2 * demucsOther]float32{.25, 0, 0, .125, 0, .25}
		dry[2*i], dry[2*i+1] = .5, .375
	}
	source := automixStemSource{timeline: &beatTimeline{}, rate: rate, dry: dry, split: split}
	cut := automixNativeCut{outBeat: 4 * time.Second, inBeat: 2 * time.Second, lead: 800 * time.Millisecond, tail: 450 * time.Millisecond,
		oldVoiceEnd: 3650 * time.Millisecond, newVoiceStart: 1900 * time.Millisecond, bassFade: 110 * time.Millisecond}
	mix := renderAutomixNative(cut, source, source)
	if mix == nil {
		t.Fatal("valid native cut did not render")
	}
	frame := func(a *automixLoopAudio, ms int) [2 * demucsStemCount]float32 {
		return a.frames[int64(ms*rate/1000)-a.first]
	}
	old, early := frame(mix.out, 3550), frame(mix.in, 1550)
	if old[0] != .25 || early[0] != 0 || early[3] != 0 || early[5] != 0 || early[6] <= 0 || early[6] >= .10 {
		t.Fatalf("preview did not isolate the incoming backing or let the old line finish: %v, %v", old, early)
	}
	if sum := old[6] + early[6]; sum < .2499 || sum > .2501 {
		t.Fatalf("the backing gained or lost level during the overlap: %v", sum)
	}
	if pickup := frame(mix.in, 1930); pickup[0] != .25 || pickup[3] != 0 || pickup[5] != 0 {
		t.Fatalf("the pickup was clipped or brought in the rhythm early: %v", pickup)
	}
	old, next := frame(mix.out, 4000), frame(mix.in, 2040)
	if old[0] != 0 || old[3] != 0 || old[5] != 0 || old[6] == 0 || next[3] != .125 || next[5] != .25 {
		t.Fatalf("the first beat failed to hand off the rhythm: %v, %v", old, next)
	}
	first := mix.in.first
	pcm := []float32{.5, .375}
	mix.in.apply(pcm, nil, nil, first, demucsGains{})
	if pcm[0] != 0 || pcm[1] != 0 {
		t.Fatalf("unprocessed drums leaked at the pre-roll seek: %v", pcm)
	}
	pcm[0], pcm[1] = .5, .375
	mix.in.apply(pcm, nil, nil, int64(3*rate), demucsGains{})
	if pcm[0] != .5 || pcm[1] != .375 {
		t.Fatalf("normal playback did not resume after the overlay: %v", pcm)
	}
}
