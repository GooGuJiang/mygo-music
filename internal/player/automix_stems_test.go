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
	plan.stems = planAutomixStems(grid, &plan)
	if got := plan.stems; got.duck != 500*time.Millisecond || got.drums != 2500*time.Millisecond || got.vocal != 2750*time.Millisecond {
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
	if outgoing := out.gains(7500, base); !(outgoing[demucsDrums] < base[demucsDrums]) || outgoing[demucsBass] != base[demucsBass] {
		t.Fatalf("the outgoing drums did not give way independently of bass: %v", outgoing)
	}
	if arrived := in.gains(2750, base); arrived != base {
		t.Fatalf("the next singer did not return to the user's mix at entry: %v", arrived)
	}
}

// The stereo decoder applies vocal ducking to the separated waveform and restores the original mix when the handoff is cleared.
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
	}
	base := &dolbyTestStream{samples: pcm}
	s := &demucsStream{stream: base, mix: 1,
		current: demucsBlock{request: request, start: 48000, bed: packets},
		next:    demucsBlock{request: request, start: 48000 + 2*frames}}
	s.request.Store(request)
	s.position.Store(48000)
	w := &watched{stream: s, rhythm: s}
	plan := &automixPlan{target: time.Second, overlap: 2 * time.Second, count: 1,
		bars:  [automixBars]automixBar{{out: 2 * time.Second, in: 2 * time.Second, speed: 1}},
		stems: automixStemTiming{duck: 500 * time.Millisecond, drums: time.Second, vocal: 1500 * time.Millisecond}}
	w.setAutomix(plan, false)
	dst, bed, weights := make([]float32, frames), make([]float32, frames/2*atmosPCMChannels), make([]float32, frames/2)
	if n, _ := s.readSurround(dst, bed, weights); n != len(dst) || math.Abs(float64(dst[0]-.35)) > 1e-6 {
		t.Fatalf("ducked PCM begins at %.4f (%d samples), want .35", dst[0], n)
	}
	w.setAutomix(nil, false)
	if n, _ := s.readSurround(dst, bed, weights); n != len(dst) || dst[0] != .5 || s.automix.Load() != nil {
		t.Fatalf("cleared handoff left the stereo level at %.4f (%d samples)", dst[0], n)
	}
}
