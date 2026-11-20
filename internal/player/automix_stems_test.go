package player

import (
	"math"
	"testing"
	"time"
)

// The outgoing singer ducks before the drums trade, and the new singer returns at a sustained entrance.
func TestAutomixStemTimingFollowsVocalEntrance(t *testing.T) {
	grid := automixGrid(2*time.Second, 2*time.Second, 2*time.Second)
	grid.stems = make([]automixStem, int(grid.end/automixFeatureStep))
	for i := range grid.stems {
		grid.stems[i].known = true
		if time.Duration(i)*automixFeatureStep >= 2750*time.Millisecond {
			grid.stems[i].vocal = .8
		}
	}
	grid.stems[36].vocal = .8 // an isolated syllable cannot set the handoff
	plan := automixPlan{target: 5 * time.Second, overlap: 4 * time.Second, count: 2,
		bars: [automixBars]automixBar{{out: 2 * time.Second, in: 2 * time.Second, speed: 1, beats: 4, inBeats: 4},
			{out: 2 * time.Second, in: 2 * time.Second, speed: 1, beats: 4, inBeats: 4}}}
	plan.stems = planAutomixStems(&beatTimeline{}, grid, &plan)
	if got := plan.stems; got.out[demucsVocals].start != -500*time.Millisecond || got.in[demucsDrums].end != 2500*time.Millisecond ||
		got.in[demucsVocals].end != 2750*time.Millisecond {
		t.Fatalf("unexpected stem handoff: %+v", got)
	}
	in, out := newAutomixStemEnvelope(&plan, 1000, true), newAutomixStemEnvelope(&plan, 1000, false)
	base := demucsGains{demucsVocals: .2, demucsDrums: -.1, demucsBass: .4}
	close := func(a, b float32) bool { return math.Abs(float64(a-b)) < 1e-5 }
	if before := out.gains(4500, base); before != base {
		t.Fatalf("ducking altered the preceding audio: %v", before)
	}
	if entry := out.gains(5000, base); !close(entry[demucsVocals], -.7) || !close(entry[demucsDrums], -.1) {
		t.Fatalf("the outgoing voice should duck before its drums: %v", entry)
	}
	first, middle, traded := in.gains(0, base), in.gains(1250, base), in.gains(2500, base)
	if !(first[demucsDrums] < middle[demucsDrums] && middle[demucsDrums] < traded[demucsDrums] && close(traded[demucsDrums], base[demucsDrums])) ||
		!close(traded[demucsVocals], -.7) {
		t.Fatalf("drums did not trade while the incoming voice stayed down: %v, %v, %v", first, middle, traded)
	}
	if outgoing := out.gains(7500, base); !(outgoing[demucsDrums] < base[demucsDrums]) || !(outgoing[demucsBass] < base[demucsBass]) {
		t.Fatalf("the outgoing drums and bass did not give way: %v", outgoing)
	}
	if arrived := in.gains(2750, base); arrived[demucsVocals] != base[demucsVocals] {
		t.Fatalf("the next singer did not return to the user's mix at entry: %v", arrived)
	}
}

// The stereo decoder adjusts separated vocals, bass, and accompaniment without changing the drum level at entry.
func TestAutomixStemEnvelopeChangesStereoPCM(t *testing.T) {
	const frames = 24
	pcm := make([]float32, frames*2)
	for i := range pcm {
		pcm[i] = .5
	}
	request := &demucsRequest{}
	packets := make([]float32, frames*2*demucsFrameSamples)
	for i := 0; i < len(packets); i += demucsFrameSamples {
		packets[i+10], packets[i+11] = .2, .2
		packets[i+12], packets[i+13] = .13, .13
		packets[i+14], packets[i+15] = .1, .1
	}
	base := &dolbyTestStream{samples: pcm}
	s := &demucsStream{stream: base, mix: 1,
		current: demucsBlock{request: request, start: 48000, bed: packets},
		next:    demucsBlock{request: request, start: 48000 + 2*frames}}
	s.request.Store(request)
	s.position.Store(48000)
	w := &watched{stream: s, rhythm: s}
	plan := &automixPlan{target: time.Second, overlap: 2 * time.Second, count: 1,
		bars: [automixBars]automixBar{{out: 2 * time.Second, in: 2 * time.Second, speed: 1}}}
	plan.stems.out[demucsVocals] = automixStemRamp{-500 * time.Millisecond, 0, .75}
	plan.stems.out[demucsDrums] = automixStemRamp{0, time.Second, .45}
	plan.stems.out[demucsBass] = automixStemRamp{-200 * time.Millisecond, 0, .60}
	plan.stems.out[demucsOther] = automixStemRamp{-200 * time.Millisecond, 0, .40}
	w.setAutomix(plan, false)
	dst, bed, weights := make([]float32, frames), make([]float32, frames/2*atmosPCMChannels), make([]float32, frames/2)
	if n, _ := s.readSurround(dst, bed, weights); n != len(dst) || math.Abs(float64(dst[0]-.262)) > 1e-6 {
		t.Fatalf("ducked PCM begins at %.4f (%d samples), want .262", dst[0], n)
	}
	w.setAutomix(nil, false)
	if n, _ := s.readSurround(dst, bed, weights); n != len(dst) || dst[0] != .5 || s.automix.Load() != nil {
		t.Fatalf("cleared handoff left the stereo level at %.4f (%d samples)", dst[0], n)
	}
}

// The low end follows the beat nearest the change of bass energy, instead of using a fixed midpoint.
func TestAutomixBassHandoffFollowsAvailableEnergy(t *testing.T) {
	const overlap, beat = 4 * time.Second, 500 * time.Millisecond
	beats := []time.Duration{beat, 2 * beat, 3 * beat, 4 * beat, 5 * beat, 6 * beat, 7 * beat}
	preferred := automixStemRamp{1750 * time.Millisecond, 2250 * time.Millisecond, .65}
	choose := func(switchAt time.Duration) time.Duration {
		var samples []automixStemSample
		for at := 50 * time.Millisecond; at < overlap; at += 100 * time.Millisecond {
			sample := automixStemSample{at: at}
			if at < switchAt {
				sample.out.power[demucsBass] = .06
			} else {
				sample.in.power[demucsBass] = .06
			}
			samples = append(samples, sample)
		}
		ramp := chooseAutomixRamp(samples, beats, overlap, overlap, beat, demucsBass, preferred, .88)
		return (ramp.start + ramp.end) / 2
	}
	early, late := choose(1400*time.Millisecond), choose(2600*time.Millisecond)
	if early >= 2*time.Second || late <= 2*time.Second || early%beat != 0 || late%beat != 0 {
		t.Fatalf("bass switches failed to follow the energy: early %s, late %s", early, late)
	}
}

// The same aligned grid gets a quicker drum exchange when the two drum patterns interleave.
func TestAutomixDrumExchangeRespondsToRhythm(t *testing.T) {
	const bar = 2 * time.Second
	out, in := automixGrid(bar, bar, bar, bar), automixGrid(bar, bar, bar, bar)
	for _, grid := range []*beatTimeline{out, in} {
		grid.stems = make([]automixStem, int(grid.end/automixFeatureStep))
		for i := range grid.stems {
			grid.stems[i].known = true
			if i%10 < 4 {
				grid.stems[i].power[demucsDrums] = .08
			}
		}
	}
	plan := matchBars(out, in, out.end, 0)
	matching := plan.stems.in[demucsDrums]
	for i := range in.stems {
		in.stems[i].power[demucsDrums] = out.stems[(i+5)%len(out.stems)].power[demucsDrums]
	}
	plan = matchBars(out, in, out.end, 0)
	mismatch := plan.stems.in[demucsDrums]
	if !plan.stems.analyzed || mismatch.end-mismatch.start >= matching.end-matching.start {
		t.Fatalf("matching drum ramp %+v, interleaved drum ramp %+v", matching, mismatch)
	}
}

// Competing accompaniment chords call for a shorter, deeper exchange even when the global key is the same.
func TestAutomixBackingHandoffRespondsToLocalHarmony(t *testing.T) {
	const bar = 2 * time.Second
	out, in := automixGrid(bar, bar, bar, bar), automixGrid(bar, bar, bar, bar)
	for _, grid := range []*beatTimeline{out, in} {
		grid.stems = make([]automixStem, int(grid.end/automixFeatureStep))
		for i := range grid.stems {
			grid.stems[i].known = true
			grid.stems[i].power[demucsOther] = .04
			grid.stems[i].chroma[0] = 1
		}
	}
	plan := matchBars(out, in, out.end, 0)
	compatible := plan.stems.in[demucsOther]
	for i := range in.stems {
		in.stems[i].chroma[0], in.stems[i].chroma[6] = 0, 1
	}
	plan = matchBars(out, in, out.end, 0)
	clash := plan.stems.in[demucsOther]
	if !plan.stems.analyzed || clash.depth <= compatible.depth || clash.end-clash.start >= compatible.end-compatible.start {
		t.Fatalf("compatible backing %+v, clashing backing %+v", compatible, clash)
	}
}

// Brief stem bleed should leave a lone singer intact, while sustained competing vocals duck.
func TestAutomixVoiceDuckNeedsSustainedOverlap(t *testing.T) {
	const bar = 2 * time.Second
	out, in := automixGrid(bar, bar, bar, bar), automixGrid(bar, bar, bar, bar)
	for _, grid := range []*beatTimeline{out, in} {
		grid.stems = make([]automixStem, int(grid.end/automixFeatureStep))
		for i := range grid.stems {
			grid.stems[i].known = true
			if grid == out {
				grid.stems[i].vocal = .9
			}
		}
	}
	in.stems[11].vocal = 1
	plan := matchBars(out, in, out.end, 0)
	if plan.count == 0 || plan.stems.out[demucsVocals].depth != 0 {
		t.Fatalf("isolated bleed ducked the singer: %+v", plan.stems)
	}
	for i := range in.stems {
		in.stems[i].vocal = .9
	}
	plan = matchBars(out, in, out.end, 0)
	if plan.count == 0 || plan.stems.out[demucsVocals].depth < .7 {
		t.Fatalf("sustained vocal overlap was not ducked: %+v", plan.stems)
	}
}
