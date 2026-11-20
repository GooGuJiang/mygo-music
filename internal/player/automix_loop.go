package player

import (
	"math"
	"time"
)

const (
	automixLoopMax        = 6 * time.Second
	automixLoopTail       = 320 * time.Millisecond
	automixLoopSeam       = 8 * time.Millisecond
	automixLoopDecodeLead = 1500 * time.Millisecond // the player's ring, output latency and Dolby block
)

type automixLoopCut struct {
	start, end time.Duration // one downbeat-to-downbeat excerpt, repeated once
	voice      time.Duration // vocal fade before the repeat
	cue        time.Duration // incoming downbeat
	beat       time.Duration
	bars       int
}

func (c automixLoopCut) target() time.Duration { return c.end + (c.end - c.start) }

func automixLoopCandidates(out *beatTimeline, end time.Duration) []automixLoopCut {
	from := automixDownbeats(out)
	var candidates []automixLoopCut
	for i := 0; i+1 < len(from); i++ {
		for bars := 1; bars <= 2 && i+bars < len(from); bars++ {
			old := out.events[from[i] : from[i+bars]+1]
			start, seam := old[0].at, old[len(old)-1].at
			span := seam - start
			tail := end - (seam + span + automixLoopTail)
			if span > automixLoopMax || tail < 180*time.Millisecond || tail > automixOverlap || !reliableAutomixGrid(old) {
				continue
			}
			beat := span / time.Duration(len(old)-1)
			if (old[1].at - start - (seam - old[len(old)-2].at)).Abs() > beat/5 {
				continue
			}
			candidates = append(candidates, automixLoopCut{start: start, end: seam, voice: min(beat, 650*time.Millisecond), beat: beat, bars: bars})
		}
	}
	return candidates
}

// Only use a loop when even half- or double-time matching would require a large tempo change.
func planAutomixLoop(out, in *beatTimeline, end time.Duration) automixLoopCut {
	loops, to := automixLoopCandidates(out, end), automixDownbeats(in)
	inPhrases := in.phraseScores(to)
	var best automixLoopCut
	bestScore := math.Inf(-1)
	for j := 0; j+1 < len(to); j++ {
		new := in.events[to[j] : to[j+1]+1]
		cue := new[0].at
		if cue > 6*time.Second {
			break
		}
		if !reliableAutomixGrid(new) {
			continue
		}
		inBeat := (new[len(new)-1].at - cue) / time.Duration(len(new)-1)
		entry, ok := automixLoopFeatures(in, cue, cue+min(2*inBeat, time.Second))
		if !ok || cue > 0 && in.stemAt(cue-100*time.Millisecond).vocal > .55 && in.stemAt(cue).vocal > .55 {
			continue
		}
		for _, loop := range loops {
			start, seam, outBeat := loop.start, loop.end, loop.beat
			if automixLoopCanStretch(outBeat, inBeat) {
				continue
			}
			body, bodyOK := automixLoopFeatures(out, start-automixLoopSeam, seam)
			carry, carryOK := automixLoopFeatures(out, seam, seam+automixLoopTail)
			if !bodyOK || !carryOK || body.vocal > .38 || body.power[demucsDrums] < .00015 {
				continue
			}
			key := harmonicMatch(estimateKey(body.chroma), estimateKey(entry.chroma))
			clash := backingClash(body, entry, key)
			if body.chroma == [12]float64{} || entry.chroma == [12]float64{} || key < 0 || clash > .58 || key == 0 && clash > .32 {
				continue
			}
			carryClash := backingClash(carry, entry, key)
			if carry.power[demucsOther] > .0007 && carryClash > .55 {
				continue
			}
			// Prefer a wrap with similar harmony and drum energy on the two sides of the seam.
			before, _ := automixLoopFeatures(out, seam-outBeat, seam)
			after, _ := automixLoopFeatures(out, start, start+outBeat)
			drumJump := math.Abs(math.Log((before.power[demucsDrums] + .00015) / (after.power[demucsDrums] + .00015)))
			if drumJump > 1.6 {
				continue
			}
			tail := end - (loop.target() + automixLoopTail)
			score := .25*float64(key) - clash - .5*carryClash - body.vocal - .2*drumJump -
				.25*backingClash(before, after, key) + .75*inPhrases[j] - .10*(tail+cue).Seconds() - .08*float64(loop.bars-1)
			if score > bestScore {
				loop.cue = cue
				best, bestScore = loop, score
			}
		}
	}
	return best
}

func automixLoopCanStretch(out, in time.Duration) bool {
	ratio := float64(out) / float64(in)
	for _, speed := range [...]float64{ratio, ratio / 2, 2 * ratio} {
		if speed >= automixMinSpeed && speed <= automixMaxSpeed {
			return true
		}
	}
	return false
}

func automixLoopFeatures(t *beatTimeline, start, end time.Duration) (automixStem, bool) {
	var avg automixStem
	var count, tonal float64
	for at := start; at < end; at += automixFeatureStep {
		stem := t.stemAt(at)
		if !stem.known {
			return avg, false
		}
		avg.vocal += stem.vocal
		for i, power := range stem.power {
			avg.power[i] += power
		}
		if stem.chroma != [12]float64{} {
			for i, strength := range stem.chroma {
				avg.chroma[i] += strength
			}
			tonal++
		}
		count++
	}
	for i := range avg.chroma {
		avg.chroma[i] /= max(1, tonal)
	}
	for i := range avg.power {
		avg.power[i] /= max(1, count)
	}
	avg.vocal /= max(1, count)
	return avg, count > 0
}

type automixLoopAudio struct {
	cut    automixLoopCut
	first  int64
	rate   int
	resume bool
	frames [][2 * demucsStemCount]float32
}

type automixStemSource struct {
	timeline    *beatTimeline
	rate, first int
	dry         []float32
	split       [][2 * demucsOther]float32
}

func (s automixStemSource) render(from, end time.Duration, c automixLoopCut, resume bool, gainsAt func(time.Duration) [demucsStemCount]float32) *automixLoopAudio {
	frameAt := func(at time.Duration) int {
		return int(math.Round((at - s.timeline.start).Seconds() * float64(s.rate)))
	}
	begin, finish := frameAt(from), frameAt(end)
	start, seam, length, first, limit := 0, finish, 0, begin, finish
	edge := max(1, s.rate*int(automixLoopSeam/time.Millisecond)/1000)
	if c.end > c.start {
		start, seam = frameAt(c.start), frameAt(c.end)
		length = seam - start
		first, limit = min(begin, start-edge), max(seam, finish-length)
		if length <= edge || finish <= seam {
			return nil
		}
	}
	if first < s.first || limit > min(s.first+len(s.split), len(s.dry)/2) || finish <= begin {
		return nil
	}
	a := &automixLoopAudio{cut: c, first: int64(s.timeline.start)*int64(s.rate)/int64(time.Second) + int64(begin), rate: s.rate,
		resume: resume, frames: make([][2 * demucsStemCount]float32, finish-begin)}
	read := func(at, stem, ch int) float32 {
		x := s.split[at-s.first]
		if stem < demucsOther {
			return x[2*stem+ch]
		}
		return s.dry[2*at+ch] - x[ch] - x[2+ch] - x[4+ch]
	}
	for i := begin; i < finish; i++ {
		gains := gainsAt(from + time.Duration(i-begin)*time.Second/time.Duration(s.rate))
		source := i
		if i >= seam {
			source -= length
		}
		for stem, gain := range gains {
			if gain == 0 {
				continue
			}
			for ch := range 2 {
				sample := read(source, stem, ch)
				if length != 0 && i >= seam-edge && i < seam {
					blend := float32(automixRampProgress(int64(i), int64(seam-edge), int64(seam)))
					sample += blend * (read(i-length, stem, ch) - sample)
				}
				a.frames[i-begin][stem*2+ch] = gain * sample
			}
		}
	}
	return a
}

func renderAutomixLoop(c automixLoopCut, t *beatTimeline, rate, first int, dry []float32, split [][2 * demucsOther]float32) *automixLoopAudio {
	s := automixStemSource{t, rate, first, dry, split}
	begin, pivot := c.end-c.voice, c.target()
	return s.render(begin, pivot+automixLoopTail, c, false, func(at time.Duration) [demucsStemCount]float32 {
		return [demucsStemCount]float32{
			1 - nativeRamp(at, begin, c.end-automixLoopSeam),
			1 - nativeRamp(at, pivot-75*time.Millisecond, pivot),
			1 - nativeRamp(at, pivot-240*time.Millisecond, pivot),
			(1 - .72*nativeRamp(at, pivot-100*time.Millisecond, pivot)) * (1 - nativeRamp(at, pivot, pivot+automixLoopTail)),
		}
	})
}

// Supply the pre-separated repeat before the ordinary Dolby processing path.
func (a *automixLoopAudio) apply(stereo, bed, weights []float32, first int64, gains demucsGains) {
	edge := max(1, a.rate*20/1000)
	for i := int(max(0, a.first-first)); i < len(stereo)/2; i++ {
		at := int(first-a.first) + i
		blend := float32(automixRampProgress(int64(at), 0, int64(edge)))
		if a.resume {
			if at >= len(a.frames) {
				break
			}
			blend = float32(automixRampProgress(int64(len(a.frames)-at), 0, int64(edge)))
		}
		var mix [2]float32
		if at < len(a.frames) {
			for stem, gain := range gains {
				for ch := range mix {
					mix[ch] += (1 + gain) * a.frames[at][2*stem+ch]
				}
			}
		}
		for ch, sample := range mix {
			stereo[2*i+ch] += blend * (sample - stereo[2*i+ch])
		}
		if len(bed) != 0 {
			row := bed[i*atmosPCMChannels : (i+1)*atmosPCMChannels]
			for ch := range row {
				row[ch] *= 1 - blend
			}
			row[0] += blend * mix[0]
			row[1] += blend * mix[1]
			weights[i] *= 1 - blend
		}
	}
}
