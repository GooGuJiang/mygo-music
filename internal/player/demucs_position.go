package player

import "math"

const (
	placementVocalCenter = 0
	placementVocalSide   = 1
	placementBassCenter  = 2
	placementDrumRear    = 3
	placementDrumTop     = 5
	placementOtherSide   = 7
	placementOtherRear   = 9
	placementOtherTop    = 11
	placementFadeMillis  = 250
	demucsFoldGain       = 0.70710678
)

// Deltas from the fixed routing. Paired sends store left and right consecutively.
type demucsPlacement [13]float32

type stereoMoments struct{ left, right, cross float64 }

func (m *stereoMoments) add(left, right float32) {
	l, r := float64(left), float64(right)
	m.left += l * l
	m.right += r * r
	m.cross += l * r
}

func (m stereoMoments) image(floor float64) (pan, width, confidence float32) {
	energy := m.left + m.right
	if energy <= floor || math.IsNaN(energy) || math.IsInf(energy, 0) {
		return 0, 0, 0
	}
	pan = float32((m.right - m.left) / energy)
	// Remove the width caused by unequal channel levels in a coherent, panned source.
	width = float32(math.Sqrt(max(0, min(1, 2*(math.Sqrt(m.left*m.right)-max(0, m.cross))/energy))))
	return pan, width, float32(min(1, (energy-floor)/(3*floor)))
}

func analyzeDemucsPlacement(bed []float32) demucsPlacement {
	var stems [4]stereoMoments
	var observations int
	// Each sampled frame retains both channels and their phase relationship.
	for i := 0; i+demucsFrameSamples <= len(bed); i += 4 * demucsFrameSamples {
		x := bed[i : i+demucsFrameSamples]
		stems[demucsVocals].add(x[10], x[11])
		stems[demucsDrums].add(x[12], x[13])
		stems[demucsBass].add(x[14], x[15])
		left := x[0] + demucsFoldGain*(x[2]+x[4]+x[6]+x[8]) - x[10] - x[12] - x[14]
		right := x[1] + demucsFoldGain*(x[2]+x[5]+x[7]+x[9]) - x[11] - x[13] - x[15]
		stems[demucsOther].add(left, right)
		observations++
	}
	var energy float64
	for _, stem := range stems {
		energy += stem.left + stem.right
	}
	floor := max(1e-8*float64(observations), 0.002*energy)
	voicePan, voiceWidth, voiceConfidence := stems[demucsVocals].image(floor)
	drumPan, drumWidth, drumConfidence := stems[demucsDrums].image(floor)
	bassPan, bassWidth, bassConfidence := stems[demucsBass].image(floor)
	otherPan, otherWidth, otherConfidence := stems[demucsOther].image(floor)
	voiceBias, bassBias := max(voicePan, -voicePan), max(bassPan, -bassPan)

	// SPHERE's placement principles anchor the lead and bass, raise high-frequency
	// parts and use the rear for ambience. The sends already isolate those bands.
	var placement demucsPlacement
	placement[placementVocalCenter] = voiceConfidence * (0.28*(1-voiceWidth)*(1-voiceBias) - 0.30*voiceBias)
	placement[placementVocalSide] = voiceConfidence * 0.08 * voiceWidth
	placement[placementBassCenter] = bassConfidence * 0.12 * (1 - bassWidth) * (1 - bassBias)
	for ch := range 2 {
		direction := float32(2*ch - 1)
		drumSide := 1 + 0.18*direction*drumPan
		otherSide := 1 + 0.22*direction*otherPan
		placement[placementDrumRear+ch] = drumConfidence * ((0.55+0.36*drumWidth)*drumSide - 1)
		placement[placementDrumTop+ch] = drumConfidence * ((0.35+0.60*drumWidth)*drumSide - 1)
		placement[placementOtherSide+ch] = otherConfidence * ((0.88+0.65*otherWidth)*otherSide - 1)
		placement[placementOtherRear+ch] = otherConfidence * 0.46 * otherWidth * otherSide
		placement[placementOtherTop+ch] = otherConfidence * ((0.65+0.68*otherWidth)*otherSide - 1)
	}
	return placement
}

func addDemucsPlacement(dst, src []float32, wet float32, gains demucsGains, p *demucsPlacement) {
	voice := wet * (1 + gains[demucsVocals])
	drums := wet * (1 + gains[demucsDrums])
	other := wet * (1 + gains[demucsOther])
	bass := wet * (1 + gains[demucsBass])
	center := voice*p[placementVocalCenter]*src[2] + bass*p[placementBassCenter]*0.5*(src[14]+src[15])
	voiceSide := voice * p[placementVocalSide] * 0.5 * (src[10] - src[11])
	dst[2] += center
	for ch := range 2 {
		side := other*p[placementOtherSide+ch]*src[6+ch] + float32(1-2*ch)*voiceSide
		rear := drums*p[placementDrumRear+ch]*src[4+ch] + other*p[placementOtherRear+ch]*src[6+ch]
		drumTop := src[16+ch]
		top := drums*p[placementDrumTop+ch]*drumTop + other*p[placementOtherTop+ch]*(src[8+ch]-drumTop)
		dst[4+ch] += rear
		dst[6+ch] += side
		dst[8+ch] += top
		// Keep the stereo fold-down and the user's independent stem gains.
		dst[ch] -= demucsFoldGain * (center + side + rear + top)
	}
}
