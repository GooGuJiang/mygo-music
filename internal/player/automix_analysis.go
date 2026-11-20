package player

import (
	"context"
	"math"
	"slices"
	"time"
)

const automixFeatureStep = 50 * time.Millisecond

type automixEnergy struct{ level, low, high float64 }
type automixStem struct {
	power  [demucsStemCount]float64
	chroma [12]float64
	vocal  float64
	known  bool
}

type automixPair struct {
	out, in    *beatTimeline
	stemsReady bool
	loop       *automixLoopAudio
}

func analyzeAutomixPair(ctx context.Context, engine *demucsEngine, out, in stream, end time.Duration, publish func(*automixPair)) {
	a, dryA, err := detectBeats(ctx, engine, out, max(0, end-automixWindow), automixWindow)
	if err != nil || len(automixDownbeats(a)) < 2 {
		return
	}
	b, dryB, err := detectBeats(ctx, engine, in, 0, automixWindow)
	if err != nil || len(automixDownbeats(b)) < 2 {
		return
	}
	a.tones, a.energy = analyzeTones(ctx, dryA, out.SampleRate(), a.start), analyzeAutomixEnergy(ctx, dryA, out.SampleRate())
	b.tones, b.energy = analyzeTones(ctx, dryB, in.SampleRate(), b.start), analyzeAutomixEnergy(ctx, dryB, in.SampleRate())
	if ctx.Err() != nil {
		return
	}
	matched := matchBars(a, b, end, 0).count > 0
	loopable := !matched && a.end+100*time.Millisecond >= end
	if !matched && !loopable {
		publish(&automixPair{out: a, in: b, stemsReady: true})
		return
	}
	publish(&automixPair{out: a, in: b})

	rateA, rateB := out.SampleRate(), in.SampleRate()
	stemsA := make([]automixStem, len(dryA)/(2*(rateA/20)))
	stemsB := make([]automixStem, len(dryB)/(2*(rateB/20)))
	// For loop candidates, keep the last two spans plus room on each side of the wrap.
	var split [][2 * demucsOther]float32
	first := len(dryA) / 2
	if loopable {
		first = max(0, first-16*rateA)
		split = make([][2 * demucsOther]float32, len(dryA)/2-first)
	}
	var loop *automixLoopAudio
	const block = 40 // Two seconds of features fit one padded Demucs request.
	for offset := 0; offset < max(len(stemsA), len(stemsB)) && ctx.Err() == nil; offset += block {
		// Start with the end of the outgoing song and the beginning of the incoming song.
		aEnd, bStart := max(0, len(stemsA)-offset), min(offset, len(stemsB))
		aStart, bEnd := max(0, aEnd-block), min(bStart+block, len(stemsB))
		okA := analyzeAutomixStemBlock(ctx, engine, dryA, rateA, aStart, stemsA[aStart:aEnd], first, split)
		okB := analyzeAutomixStemBlock(ctx, engine, dryB, rateB, bStart, stemsB[bStart:bEnd], 0, nil)
		if ctx.Err() != nil {
			return
		}
		finished := offset+block >= max(len(stemsA), len(stemsB)) || !okA || !okB
		withStemsA, withStemsB := *a, *b
		withStemsA.stems, withStemsB.stems = slices.Clone(stemsA), slices.Clone(stemsB)
		if loopable {
			cut := planAutomixLoop(&withStemsA, &withStemsB, end)
			if cut.end > cut.start && (loop == nil || loop.cut != cut) {
				loop = renderAutomixLoop(cut, a, rateA, first, dryA, split)
			}
		}
		publish(&automixPair{out: &withStemsA, in: &withStemsB, stemsReady: finished, loop: loop})
		if finished {
			return
		}
	}
}

func analyzeAutomixEnergy(ctx context.Context, pcm []float32, rate int) []automixEnergy {
	hop := max(1, rate/20)
	lowStep := 1 - math.Exp(-2*math.Pi*180/float64(rate))
	highStep := 1 - math.Exp(-2*math.Pi*2000/float64(rate))
	var low, upper [2]float64
	energy := make([]automixEnergy, 0, len(pcm)/(2*hop))
	for offset := 0; offset+2*hop <= len(pcm) && ctx.Err() == nil; offset += 2 * hop {
		var sum automixEnergy
		for i, sample := range pcm[offset : offset+2*hop] {
			x, ch := float64(sample), i%2
			if math.IsNaN(x) || math.IsInf(x, 0) {
				continue
			}
			low[ch] += lowStep * (x - low[ch])
			upper[ch] += highStep * (x - upper[ch])
			sum.level += x * x
			sum.low += low[ch] * low[ch]
			sum.high += (x - upper[ch]) * (x - upper[ch])
		}
		samples := float64(2 * hop)
		energy = append(energy, automixEnergy{sum.level / samples, sum.low / samples, sum.high / samples})
	}
	return energy
}

func (t *beatTimeline) accent(at time.Duration) float64 {
	index := int((at - t.start + automixFeatureStep/2) / automixFeatureStep)
	if index < 0 || index >= len(t.energy) {
		return 0
	}
	level := func(i int) float64 {
		x := t.energy[i]
		mix := math.Sqrt(.65*x.low + .35*x.level)
		if i < len(t.stems) && t.stems[i].known {
			return .35*mix + .65*math.Sqrt(t.stems[i].power[demucsDrums])
		}
		return mix
	}
	before, after := level(max(0, index-1)), level(index)
	return max(0, after-before) + .2*after
}

// A change that persists across neighboring bars is more useful than a single loud beat.
func (t *beatTimeline) phraseScores(bars []int) []float64 {
	scores := make([]float64, len(bars))
	for i := 1; i+1 < len(bars); i++ {
		start, at, end := t.events[bars[i-1]].at, t.events[bars[i]].at, t.events[bars[i+1]].at
		average := func(a, b time.Duration) automixEnergy {
			lo := int((a - t.start) / automixFeatureStep)
			hi := int((b - t.start) / automixFeatureStep)
			var sum automixEnergy
			if lo < 0 || hi > len(t.energy) || lo >= hi {
				return sum
			}
			for _, x := range t.energy[lo:hi] {
				sum.level += x.level
				sum.low += x.low
				sum.high += x.high
			}
			n := float64(hi - lo)
			return automixEnergy{sum.level / n, sum.low / n, sum.high / n}
		}
		before, after := average(start, at), average(at, end)
		if max(before.level, after.level) < 1e-6 {
			continue
		}
		volume := min(1, math.Abs(math.Log((before.level+1e-6)/(after.level+1e-6)))/3)
		color := min(1, math.Abs(before.low/(before.level+1e-6)-after.low/(after.level+1e-6))+
			math.Abs(before.high/(before.level+1e-6)-after.high/(after.level+1e-6)))
		var left, right [12]float64
		var nl, nr float64
		for _, tone := range t.tones {
			switch {
			case tone.at >= start && tone.at < at:
				for j, x := range tone.chroma {
					left[j] += x
				}
				nl++
			case tone.at >= at && tone.at < end:
				for j, x := range tone.chroma {
					right[j] += x
				}
				nr++
			}
		}
		var harmony float64
		if nl > 0 && nr > 0 {
			for j := range left {
				harmony += .5 * math.Abs(left[j]/nl-right[j]/nr)
			}
		}
		scores[i] = .55*volume + .25*color + .20*harmony
	}
	return scores
}

func analyzeAutomixStemBlock(ctx context.Context, engine *demucsEngine, pcm []float32, rate, first int, stems []automixStem, splitFirst int, split [][2 * demucsOther]float32) bool {
	if len(stems) == 0 {
		return true
	}
	hop, pad := rate/20, rate/8
	start, end := first*hop, (first+len(stems))*hop
	lo, hi := max(0, start-pad), min(end+pad, len(pcm)/2)
	result := engine.request(demucsJob{ctx: ctx, rate: rate, stereo: pcm[2*lo : 2*hi], analysis: true})
	if result.err != nil || len(result.bed) != (hi-lo)*demucsFrameSamples {
		return false
	}
	// The outgoing blocks arrive from the tail back. Blend this block's padding into its already separated neighbor.
	for i := max(start, splitFirst); i < min(hi, splitFirst+len(split), len(pcm)/2/hop*hop); i++ {
		x := result.bed[(i-lo)*demucsFrameSamples+atmosPCMChannels:]
		dst := &split[i-splitFirst]
		if i < end {
			copy(dst[:], x[:2*demucsOther])
		} else {
			blend := float32(automixRampProgress(int64(i), int64(end), int64(hi)))
			for ch := range dst {
				dst[ch] = x[ch] + blend*(dst[ch]-x[ch])
			}
		}
	}
	backing := make([]float32, 2*(hi-lo))
	for i := lo; i < hi; i++ {
		x := result.bed[(i-lo)*demucsFrameSamples:]
		for ch := range 2 {
			dry := pcm[2*i+ch]
			if math.IsNaN(float64(dry)) || math.IsInf(float64(dry), 0) {
				continue
			}
			// The native packet has no raw Other pair. The stereo mixer assigns the residual to Other.
			other := dry - x[10+ch] - x[12+ch] - x[14+ch]
			backing[2*(i-lo)+ch] = other + .5*x[14+ch]
		}
	}
	toneStart := time.Duration(lo) * time.Second / time.Duration(rate)
	tones := analyzeTones(ctx, backing, rate, toneStart)
	nextTone := 0
	for bin := range stems {
		frame := start + bin*hop
		var total float64
		var power [demucsStemCount]float64
		for i := frame; i < frame+hop; i += 4 {
			x := result.bed[(i-lo)*demucsFrameSamples:]
			for ch := range 2 {
				dry := float64(pcm[2*i+ch])
				if math.IsNaN(dry) || math.IsInf(dry, 0) {
					continue
				}
				total += dry * dry
				for stem := range demucsOther {
					sample := float64(x[atmosPCMChannels+2*stem+ch])
					power[stem] += sample * sample
					dry -= sample
				}
				power[demucsOther] += dry * dry
			}
		}
		samples := float64(2 * ((hop + 3) / 4))
		voice := power[demucsVocals] / samples
		strength := min(1, max(0, (10*math.Log10(voice+1e-12)+40)/16))
		for i := range power {
			power[i] /= samples
		}
		stems[bin] = automixStem{vocal: strength * min(1, math.Sqrt(voice/(total/samples+1e-8))/.27), power: power, known: true}
		at := time.Duration(frame+hop/2) * time.Second / time.Duration(rate)
		for nextTone+1 < len(tones) && (tones[nextTone+1].at-at).Abs() <= (tones[nextTone].at-at).Abs() {
			nextTone++
		}
		if nextTone < len(tones) && (tones[nextTone].at-at).Abs() <= 300*time.Millisecond {
			stems[bin].chroma = tones[nextTone].chroma
		}
	}
	return true
}

func (t *beatTimeline) stemAt(at time.Duration) automixStem {
	i := int((at - t.start) / automixFeatureStep)
	if at < t.start || i >= len(t.stems) {
		return automixStem{}
	}
	return t.stems[i]
}
