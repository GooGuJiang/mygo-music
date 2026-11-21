package player

import (
	"math"
	"testing"
	"time"
)

// Only carry the old bass while the incoming low end needs it, and end the carry when the incoming bass arrives.
func TestAutomixLowpassAdaptsBassCarry(t *testing.T) {
	const cue, pivot = time.Second, 4 * time.Second
	in := &beatTimeline{start: cue, stems: make([]automixStem, 8)}
	for i := range in.stems {
		in.stems[i].known = true
		if i >= 4 {
			in.stems[i].power[demucsBass] = .02
		}
	}
	in.stems[0].power[demucsDrums] = .06
	old := automixStem{power: [demucsStemCount]float64{0, 0, .02}}
	if filter := planAutomixLowpass(in, cue, pivot, 450*time.Millisecond, old, old); filter.bassEnd != pivot+200*time.Millisecond || filter.bassGain <= 0 {
		t.Fatalf("a kick followed by a low-end gap needs a short carry: %+v", filter)
	}
	for i := range in.stems {
		in.stems[i].power[demucsBass] = .02
	}
	if filter := planAutomixLowpass(in, cue, pivot, 450*time.Millisecond, old, old); filter.bassGain != 0 {
		t.Fatalf("added bass on top of an already strong bass entrance: %+v", filter)
	}
}

// The outgoing low-pass tail leaves the first kick and voice to the new track while carrying only the old low end.
func TestAutomixLowpassRendersBassTail(t *testing.T) {
	const rate = 48000
	dry := make([]float32, 2*6*rate)
	split := make([][2 * demucsOther]float32, len(dry)/2)
	for i := range split {
		bass := float32(.25 * math.Sin(2*math.Pi*80*float64(i)/rate))
		bright := float32(.25 * math.Sin(2*math.Pi*2400*float64(i)/rate))
		split[i] = [2 * demucsOther]float32{.1, .1, .1, .1, bass, bass}
		dry[2*i], dry[2*i+1] = .2+bass+bright, .2+bass+bright
	}
	source := automixStemSource{timeline: &beatTimeline{}, rate: rate, dry: dry, split: split}
	cut := automixNativeCut{outBeat: 4 * time.Second, inBeat: 2 * time.Second, lead: 800 * time.Millisecond, tail: 450 * time.Millisecond,
		newVoiceStart: 2 * time.Second, bassFade: 100 * time.Millisecond}
	base := renderAutomixNative(cut, source, source).out
	cut.lowpass = automixLowpassTail{at: cut.outBeat, bassEnd: cut.outBeat + 200*time.Millisecond, bassGain: .7}
	filtered := renderAutomixNative(cut, source, source).out
	energy := func(a *automixLoopAudio, start, length, stem int) float64 {
		var sum float64
		for i := int64(start*rate/1000) - a.first; i < int64((start+length)*rate/1000)-a.first; i++ {
			x := float64(a.frames[i][2*stem])
			sum += x * x
		}
		return sum / float64(length*rate/1000)
	}
	for _, stem := range [...]int{demucsVocals, demucsDrums} {
		if got := energy(filtered, 4040, 60, stem); got != 0 {
			t.Fatalf("old stem %d leaked after the downbeat: %f", stem, got)
		}
	}
	if bass := energy(filtered, 4040, 60, demucsBass); bass < .003 || energy(filtered, 4220, 60, demucsBass) != 0 {
		t.Fatalf("the bass tail failed to carry or release: %f", bass)
	}
	if high := energy(filtered, 4040, 60, demucsOther); high > .02*energy(base, 4040, 60, demucsOther) {
		t.Fatalf("the outgoing backing retained its high frequencies: %f", high)
	}
}
