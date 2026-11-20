package player

import (
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
	grid := func(lengths ...time.Duration) *beatTimeline {
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
	out := grid(2*time.Second, 2*time.Second, 2100*time.Millisecond, 1920*time.Millisecond)
	in := grid(2*time.Second, 2040*time.Millisecond)
	plan := matchBars(out, in, out.end, 0)
	if plan.count != 2 || plan.target != 4*time.Second || plan.cue != 0 || plan.overlap != 4040*time.Millisecond {
		t.Fatalf("unexpected transition: %+v", plan)
	}
	if at, speed := plan.alignment(2 * time.Second); at != 6100*time.Millisecond || math.Abs(speed-1920.0/2040) > 1e-12 {
		t.Fatalf("second downbeat landed at %s with speed %.4f", at, speed)
	}
	if speed := plan.approach((plan.ramp + plan.target - automixLead) / 2); speed <= 1 || speed >= plan.bars[0].speed {
		t.Fatalf("tempo did not ramp before the overlap: %.4f", speed)
	}
	if plan := matchBars(out, grid(3*time.Second, 3*time.Second), out.end, 0); plan.count != 0 {
		t.Fatalf("accepted an excessive tempo change: %+v", plan)
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
	a.apply(out, 0, rate, &automixEQ{end: end})
	b.apply(in, 0, rate, &automixEQ{end: end, incoming: true})
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
