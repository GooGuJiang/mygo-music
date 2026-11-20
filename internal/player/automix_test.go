package player

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/egoist/mygo/audio"
	"mygo-music/internal/music"
)

// The progress marker follows the planned downbeat, then the incoming cue once playback switches tracks.
func TestAutomixProgressMarkerUsesTrackTime(t *testing.T) {
	p := New(func(func()) {})
	p.Cue([]music.Track{{ID: "a"}, {ID: "b"}})
	p.Source = func(string) music.Source { return &served{} }
	p.Automix = func() bool { return true }
	p.Attach(audio.NewPlayer(silence{}))
	t.Cleanup(p.out.Close)
	p.stream = &demucsStream{engine: &demucsEngine{mode: DemucsCUDA}}
	if point, estimated, ok := p.AutomixPoint(); !ok || !estimated || point != p.out.Duration()-automixOverlap {
		t.Fatalf("missing estimated point: %s, estimated=%v, ok=%v", point, estimated, ok)
	}
	plan := automixPlan{target: 595 * time.Second, cue: 200 * time.Millisecond}
	p.automixPrep = &automixPreparation{plan: plan, out: p.out}
	if point, estimated, ok := p.AutomixPoint(); !ok || estimated || point != plan.target || p.AutomixInfo().Start != plan.target {
		t.Fatalf("progress and panel did not show the planned point: %s", point)
	}
	p.automixPrep, p.automixMix = nil, &plan
	if point, estimated, ok := p.AutomixPoint(); !ok || estimated || point != plan.cue || p.AutomixInfo().Cue != plan.cue {
		t.Fatalf("the marker did not switch to the incoming cue: %s", point)
	}
}

// Each outgoing bar lands at the next incoming downbeat even when the bar durations differ.
func TestAutomixAlignsUnevenBars(t *testing.T) {
	out := automixGrid(2*time.Second, 2*time.Second, 2*time.Second, 2*time.Second, 2100*time.Millisecond, 1920*time.Millisecond)
	in := automixGrid(2*time.Second, 2040*time.Millisecond)
	plan := matchBars(out, in, out.end, 0)
	if plan.count != 2 || plan.target != 8*time.Second || plan.cue != 0 || plan.overlap != 4040*time.Millisecond {
		t.Fatalf("unexpected transition: %+v", plan)
	}
	if at, speed := plan.alignment(2 * time.Second); at != 10100*time.Millisecond || math.Abs(speed-1920.0/2040) > 1e-12 {
		t.Fatalf("second downbeat landed at %s with speed %.4f", at, speed)
	}
	if speed, next := plan.approach(1900 * time.Millisecond); speed != 1 || next != 2*time.Second {
		t.Fatalf("tempo changed before the ramp downbeat: %.4f, next %s", speed, next)
	}
	if speed, next := plan.approach(2 * time.Second); speed != 1 || next != 4*time.Second {
		t.Fatalf("first ramp bar did not start at the original tempo: %.4f, next %s", speed, next)
	}
	if speed, _ := plan.approach(4 * time.Second); speed <= 1 || speed >= plan.bars[0].speed {
		t.Fatalf("tempo did not ramp before the overlap: %.4f", speed)
	}
	if plan := matchBars(out, automixGrid(5800*time.Millisecond, 5800*time.Millisecond), out.end, 0); plan.count != 0 {
		t.Fatalf("accepted an excessive tempo change: %+v", plan)
	}
}

func automixGrid(lengths ...time.Duration) *beatTimeline {
	w := &beatTimeline{}
	for _, length := range lengths {
		for beat := range 4 {
			w.events = append(w.events, beatEvent{w.end + time.Duration(beat)*length/4, beat == 0})
		}
		w.end += length
	}
	w.events = append(w.events, beatEvent{w.end, true})
	w.end += 20 * time.Millisecond
	return w
}

// A 90-to-135 BPM pair can use all eight bars and keep the incoming track at its original tempo.
func TestAutomixUsesEightBarsAtWideTempoDifference(t *testing.T) {
	var slow, fast [9]time.Duration
	for i := range slow {
		slow[i], fast[i] = 2640*time.Millisecond, 1760*time.Millisecond
	}
	out, in := automixGrid(slow[:]...), automixGrid(fast[:]...)
	plan := matchBars(out, in, out.end, 0)
	if plan.count != 8 || plan.cue != 0 || plan.target != slow[0] || plan.overlap != 8*fast[0] || plan.bars[0].speed != 1.5 {
		t.Fatalf("expected all eight matched bars: %+v", plan)
	}
}

// The same music can transition in either direction at near-original speed when one grid counts twice as many beats.
func TestAutomixMatchesHalfAndDoubleTime(t *testing.T) {
	slow := automixGrid(3200*time.Millisecond, 3200*time.Millisecond, 3200*time.Millisecond, 3200*time.Millisecond, 3200*time.Millisecond)
	var lengths [10]time.Duration
	for i := range lengths {
		lengths[i] = 1600 * time.Millisecond
	}
	fast := automixGrid(lengths[:]...)
	for _, test := range []struct {
		out, in     *beatTimeline
		bars, beats int
	}{{slow, fast, 8, 32}, {fast, slow, 4, 16}} {
		plan := matchBars(test.out, test.in, test.out.end, 0)
		if plan.count != 4 || plan.inBars != test.bars || plan.beats != test.beats || plan.bars[0].speed != 1 || plan.overlap != 12800*time.Millisecond {
			t.Fatalf("unexpected half/double-time plan: %+v", plan)
		}
		if at, _ := plan.alignment(1600 * time.Millisecond); at != plan.target+1600*time.Millisecond {
			t.Fatalf("paired midpoint drifted to %s", at)
		}
	}
	uneven := automixGrid(1550*time.Millisecond, 1650*time.Millisecond)
	bar, _, ok := fitAutomixBar(slow, uneven, slow.events[:5], uneven.events, 1, 2)
	if at, speed := bar.stretch(1550 * time.Millisecond); !ok || at != 1600*time.Millisecond || math.Abs(speed-1.6/1.65) > 1e-12 {
		t.Fatalf("uneven half-time midpoint mapped to %s at %.4f: %+v", at, speed, bar)
	}
}

// An incoming section change can make a two-bar blend preferable to a longer overlap.
func TestAutomixEndsOverlapOnPhraseBoundary(t *testing.T) {
	var lengths [10]time.Duration
	for i := range lengths {
		lengths[i] = 2 * time.Second
	}
	out, in := automixGrid(lengths[:]...), automixGrid(lengths[:]...)
	in.energy = make([]automixEnergy, int(in.end/automixFeatureStep))
	for i := range in.energy {
		if time.Duration(i)*automixFeatureStep < 4*time.Second {
			in.energy[i] = automixEnergy{level: .008, low: .006}
		} else {
			in.energy[i] = automixEnergy{level: .18, high: .12}
		}
	}
	if plan := matchBars(out, in, out.end, 0); plan.cue != 0 || plan.inBars != 2 || plan.beats != 8 {
		t.Fatalf("did not end the fade at the phrase boundary: %+v", plan)
	}
}

// Simultaneous singing shortens an otherwise compatible blend to the instrumental introduction.
func TestAutomixAvoidsOverlappingVocals(t *testing.T) {
	var lengths [10]time.Duration
	for i := range lengths {
		lengths[i] = 2 * time.Second
	}
	out, in := automixGrid(lengths[:]...), automixGrid(lengths[:]...)
	out.stems = make([]automixStem, int(out.end/automixFeatureStep))
	in.stems = make([]automixStem, len(out.stems))
	for i := range out.stems {
		out.stems[i].vocal = 1
		if time.Duration(i)*automixFeatureStep >= 4*time.Second {
			in.stems[i].vocal = 1
		}
	}
	if plan := matchBars(out, in, out.end, 0); plan.inBars != 2 || plan.cue != 0 || automixVocalClash(out, in, &plan) != 0 {
		t.Fatalf("expected a blend confined to the intro: %+v", plan)
	}
}

// Correct downbeats alone cannot rescue an overlap whose internal beats and accents disagree.
func TestAutomixChecksBeatTimingAndAccents(t *testing.T) {
	const length = 2 * time.Second
	out, in := automixGrid(length, length, length), automixGrid(length, length, length)
	for _, grid := range []*beatTimeline{out, in} {
		grid.energy = make([]automixEnergy, int(grid.end/automixFeatureStep))
		for i := range grid.energy {
			grid.energy[i].level = .01
		}
	}
	out.energy[int(2500*time.Millisecond/automixFeatureStep)].low = .8
	in.energy[int(500*time.Millisecond/automixFeatureStep)].low = .8
	in.energy[int(3*time.Second/automixFeatureStep)].low = .8
	_, matching, good := fitAutomixBar(out, in, out.events[4:9], in.events[:5], 1, 1)
	_, clashing, bad := fitAutomixBar(out, in, out.events[4:9], in.events[4:9], 1, 1)
	if !good || !bad || matching <= clashing {
		t.Fatalf("matching strong beats scored %.3f, mismatched %.3f", matching, clashing)
	}
	for i := 1; i < len(in.events); i += 4 {
		in.events[i].at += 130 * time.Millisecond
	}
	if plan := matchBars(out, in, out.end, 0); plan.count != 0 {
		t.Fatalf("unreliable internal beat timing should use the ordinary fade: %+v", plan)
	}
}

// A compatible excerpt wins over the first cue, while a confirmed clash keeps a shorter beat-matched blend.
func TestAutomixMatchesKeysFromOverlapAudio(t *testing.T) {
	const rate = 48000
	tones := func(notes [3]int, start, duration time.Duration) []tonalFrame {
		pcm := make([]float32, 2*int(duration.Seconds()*rate))
		for i := range len(pcm) / 2 {
			var value float64
			for j, note := range notes {
				hz := 440 * math.Exp2(float64(note-69)/12)
				value += (.3 - .08*float64(j)) * math.Sin(2*math.Pi*hz*float64(i)/rate)
			}
			pcm[2*i], pcm[2*i+1] = float32(value), float32(value)
		}
		return analyzeTones(context.Background(), pcm, rate, start)
	}
	var bars [6]time.Duration
	for i := range bars {
		bars[i] = 2 * time.Second
	}
	out, in := automixGrid(bars[:]...), automixGrid(bars[:]...)
	out.tones = tones([3]int{48, 52, 55}, 0, 12*time.Second) // C major
	in.tones = tones([3]int{54, 58, 61}, 0, 12*time.Second)  // F-sharp major
	if got := out.keyBetween(0, out.end); got != 1 {
		t.Fatalf("expected C major, got key %d", got)
	}
	if got := in.keyBetween(0, in.end); got != 7 {
		t.Fatalf("expected F-sharp major, got key %d", got)
	}
	if plan := matchBars(out, in, out.end, 0); plan.count != 2 || plan.cue != 0 {
		t.Fatalf("expected a short beat-matched blend for clashing keys: %+v", plan)
	}
	in.tones = append(tones([3]int{54, 58, 61}, 0, 6*time.Second), tones([3]int{48, 52, 55}, 6*time.Second, 6*time.Second)...)
	if plan := matchBars(out, in, out.end, 0); plan.count < 2 || plan.cue == 0 || in.keyBetween(plan.cue, plan.cue+plan.overlap) != 1 {
		t.Fatalf("did not prefer a matching section: %+v", plan)
	}
	if harmonicMatch(1, 22) <= 0 || harmonicMatch(22, 1) <= 0 || harmonicMatch(1, 8) <= 0 {
		t.Fatal("relative and fifth-related keys should match")
	}
	invalid := make([]float32, 2*rate)
	invalid[0] = float32(math.NaN())
	if frames := analyzeTones(context.Background(), invalid, rate, 0); len(frames) != 0 {
		t.Fatal("silence and invalid samples should not produce pitch classes")
	}
}

// Bass moves from the outgoing track to the incoming track, with the latter back to full EQ at the seam.
func TestAutomixEQPassesBassAcross(t *testing.T) {
	const rate = 48000
	end := int64(rate)
	makeBass := func() []float32 {
		pcm := make([]float32, 2*(end+1))
		for i := range pcm {
			pcm[i] = float32(math.Sin(2 * math.Pi * 45 * float64(i/2) / rate))
		}
		return pcm
	}
	out, in := makeBass(), makeBass()
	var a, b automixBands
	plan := &automixPlan{overlap: time.Second, count: 1, bars: [automixBars]automixBar{{out: time.Second, in: time.Second, speed: 1}}}
	a.apply(out, 0, rate, newAutomixEQ(plan, rate, false))
	b.apply(in, 0, rate, newAutomixEQ(plan, rate, true))
	energy := func(pcm []float32) float64 {
		var sum float64
		for _, x := range pcm {
			sum += float64(x * x)
		}
		return sum
	}
	if energy(in[:rate/5]) >= 0.5*energy(out[:rate/5]) || energy(out[rate*9/5:]) >= 0.5*energy(in[rate*9/5:]) {
		t.Fatal("the EQ did not trade bass between the tracks")
	}
	if dry := makeBass(); in[2*end] != dry[2*end] || in[2*end+1] != dry[2*end+1] {
		t.Fatal("the incoming track stayed filtered past the overlap")
	}
}
