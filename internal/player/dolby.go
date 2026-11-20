package player

import (
	"io"
	"sync/atomic"
)

// DolbyProfile configures the DAX3 effects. Zero strengths bypass Dolby.
type DolbyProfile struct {
	Leveler                float64 `json:"leveler"`
	Target                 float64 `json:"target"`
	Dialogue               float64 `json:"dialogue"`
	IntelligentEQ          float64 `json:"intelligentEQ"`
	IntelligentEQCurve     string  `json:"intelligentEQCurve,omitempty"`
	IntelligentEQSteering  bool    `json:"intelligentEQSteering,omitempty"`
	InputTarget            float64 `json:"inputTarget,omitempty"`
	OutputGain             float64 `json:"outputGain,omitempty"`
	VolumeModeler          bool    `json:"volumeModeler,omitempty"`
	VolumeCalibration      float64 `json:"volumeCalibration,omitempty"`
	DisableSurroundDecoder bool    `json:"disableSurroundDecoder,omitempty"`
	Spatial                bool    `json:"spatial,omitempty"`
	Preset                 string  `json:"-"`
	DolbyAddOns            `json:"-"`
}

// DolbyProcessing is the decoder's last committed path and whether a change is queued.
type DolbyProcessing struct {
	Profile       DolbyProfile
	SampleRate    int
	InputChannels int
	Active        bool
	Pending       bool
}

// DolbyAddOns are shared by the built-in and custom presets.
type DolbyAddOns struct {
	BassBoost       float64     `json:"bassBoost,omitempty"`
	GraphicEQ       bool        `json:"graphicEQ,omitempty"`
	GraphicGains    [20]float64 `json:"graphicGains,omitzero"`
	DialogueDucking float64     `json:"dialogueDucking,omitempty"`
}

func (a DolbyAddOns) graphicActive() bool {
	return a.GraphicEQ && a.GraphicGains != [20]float64{}
}

func (p DolbyProfile) enabled() bool {
	return p.Spatial || p.Leveler > 0 || p.Dialogue > 0 || p.IntelligentEQ > 0 || p.OutputGain != 0 || p.VolumeModeler ||
		p.BassBoost > 0 || p.graphicActive() || p.DialogueDucking > 0
}

// DolbyEQFrequencies are the band centers in the AAF Generic DolbyAtmos tuning.
func DolbyEQFrequencies(rate int) [20]int32 {
	if rate%44100 == 0 {
		return [...]int32{43, 129, 215, 301, 431, 603, 775, 947, 1206, 1550, 2067, 2756, 3618, 4651, 5685, 7063, 8958, 11025, 13781, 18777}
	}
	return [...]int32{47, 141, 234, 328, 469, 656, 844, 1031, 1313, 1688, 2250, 3000, 3750, 4688, 5813, 7125, 9000, 11250, 13875, 19688}
}

type dolbyEffect interface {
	process(samples []float32) bool
	reset() bool
	close()
}

type dolbySurroundEffect interface {
	render(input, output []float32) bool
}

// surroundStream supplies a bed alongside the stereo stream used for bypass.
type surroundStream interface {
	surroundChannels() int
	readSurround(stereo, bed, weights []float32) (int, error)
}

// dolbyStream renders both profiles from the same decoded frames during a switch.
// Each path removes its own APO delay before its output enters the crossfade.
type dolbyStream struct {
	stream
	requested  atomic.Pointer[DolbyProfile]
	status     func(*DolbyProfile, bool, int)
	openEffect func(int, int, DolbyProfile) (dolbyEffect, int)
	clarityOn  atomic.Bool

	current, next *dolbyPath
	faded         int // stereo frames mixed in the current transition
	end           error
	dry, surround []float32 // decoded audio that has not been returned yet
	demucsWeights []float32 // one weight per queued stereo frame, including the APO's delay
	clarity       *demucsClarity
	block, wet    [2 * apoBlockFrames]float32
	raw           []float32
	channels      int
}

const (
	apoBlockFrames  = 2048
	dolbyFadeMillis = 60
)

type dolbyPath struct {
	profile     *DolbyProfile
	effect      dolbyEffect
	channels    int
	delay, skip int // interleaved stereo samples
	ready       []float32
}

func (s *dolbyStream) Read(dst []float32) (int, error) {
	size := min(len(dst)&^1, len(s.block))
	if size == 0 {
		return 0, nil
	}
	want := s.requested.Load()
	if s.current == nil {
		s.channels = 2
		if source, ok := s.stream.(surroundStream); ok {
			s.channels = source.surroundChannels()
		}
		if s.channels > 2 {
			if s.raw == nil {
				s.raw = make([]float32, apoBlockFrames*s.channels)
			}
		}
		s.current = s.openPath(want)
		s.report(s.current)
	} else if s.next == nil && want != s.current.profile {
		s.next = s.openPath(want)
		s.faded = 0
		s.feed(s.next, s.dry, s.surround)
		if s.end != nil {
			s.flush(s.next)
		}
	}

	fadeFrames := max(1, s.SampleRate()*dolbyFadeMillis/1000)
	for {
		dry := s.dry
		previous := s.current.samples(dry)
		incoming := s.next.samples(dry)
		n := min(size, len(dry), len(previous), len(incoming))
		if n > 0 {
			copy(dst[:n], previous[:n])
			clarityOn := s.clarityOn.Load()
			clarityRunning := len(s.demucsWeights) != 0 && (clarityOn || s.clarity != nil && s.clarity.mix > 0)
			currentBed := s.current.rendersBed()
			nextBed := s.next.rendersBed()
			if s.next != nil {
				for i := 0; i < n; i += 2 {
					t := float32(min(s.faded+i/2, fadeFrames)) / float32(fadeFrames)
					weight := t * t * (3 - 2*t)
					dst[i] += weight * (incoming[i] - dst[i])
					dst[i+1] += weight * (incoming[i+1] - dst[i+1])
					if clarityRunning && currentBed != nextBed {
						if currentBed {
							weight = 1 - weight
						}
						s.demucsWeights[i/2] *= weight
					}
				}
				s.faded += n / 2
			}
			if clarityRunning && (currentBed || nextBed) {
				if s.clarity == nil {
					s.clarity = newDemucsClarity(s.SampleRate())
				}
				s.clarity.process(dst[:n], s.demucsWeights[:n/2], clarityOn)
			} else if s.clarity != nil {
				s.clarity.reset()
			}
			s.current.consume(n)
			s.next.consume(n)
			s.dry = s.dry[:copy(s.dry, s.dry[n:])]
			if s.channels > 2 {
				s.surround = s.surround[:copy(s.surround, s.surround[n/2*s.channels:])]
			}
			if len(s.demucsWeights) != 0 {
				s.demucsWeights = s.demucsWeights[:copy(s.demucsWeights, s.demucsWeights[n/2:])]
			}
			if s.next != nil && s.faded >= fadeFrames {
				s.current.close()
				s.current, s.next = s.next, nil
				s.report(s.current)
			}
			return n, nil
		}
		if s.end != nil {
			return 0, s.end
		}
		s.readBlock(size)
	}
}

func (s *dolbyStream) openPath(profile *DolbyProfile) *dolbyPath {
	p := &dolbyPath{profile: profile, channels: 2}
	if profile == nil || !profile.enabled() {
		return p
	}
	open := s.openEffect
	if open == nil {
		open = openDolbyEffect
	}
	if profile.Spatial && s.channels > 2 {
		p.channels = s.channels
		p.effect, p.delay = open(s.SampleRate(), p.channels, *profile)
	}
	if p.effect == nil && p.channels != atmosPCMChannels {
		p.channels = 2
		p.effect, p.delay = open(s.SampleRate(), p.channels, *profile)
	}
	p.skip = p.delay
	return p
}

func (s *dolbyStream) report(p *dolbyPath) {
	if s.status != nil && p.profile != nil {
		s.status(p.profile, p.effect != nil, p.channels)
	}
}

func (s *dolbyStream) readBlock(size int) {
	var n int
	var surround []float32
	if s.channels > 2 {
		var weights []float32
		start := len(s.demucsWeights)
		if _, ok := s.stream.(*demucsStream); ok {
			s.demucsWeights = append(s.demucsWeights, make([]float32, size/2)...)
			weights = s.demucsWeights[start:]
		}
		n, s.end = s.stream.(surroundStream).readSurround(s.block[:size], s.raw, weights)
		if len(weights) != 0 {
			s.demucsWeights = s.demucsWeights[:start+n/2]
		}
		surround = s.raw[:n/2*s.channels]
		s.surround = append(s.surround, surround...)
	} else {
		n, s.end = s.stream.Read(s.block[:size])
	}
	if n > 0 {
		s.dry = append(s.dry, s.block[:n]...)
		s.feed(s.current, s.block[:n], surround)
		s.feed(s.next, s.block[:n], surround)
	}
	if n == 0 && s.end == nil {
		s.end = io.EOF
	}
	if s.end != nil {
		s.flush(s.current)
		s.flush(s.next)
	}
}

func (s *dolbyStream) feed(p *dolbyPath, dry, surround []float32) {
	for p != nil && p.effect != nil && len(dry) > 0 {
		n := min(len(dry), len(s.wet))
		wet := s.wet[:n]
		var ok bool
		if p.channels > 2 {
			samples := n / 2 * p.channels
			ok = p.effect.(dolbySurroundEffect).render(surround[:samples], wet)
			surround = surround[samples:]
		} else {
			copy(wet, dry[:n])
			ok = p.effect.process(wet)
		}
		if !ok {
			p.close()
			if p == s.current {
				s.report(p)
			}
			return
		}
		skip := min(p.skip, n)
		p.skip -= skip
		p.ready = append(p.ready, wet[skip:]...)
		dry = dry[n:]
	}
}

func (s *dolbyStream) flush(p *dolbyPath) {
	if p == nil || p.effect == nil {
		return
	}
	clear(s.block[:])
	clear(s.raw)
	for left := p.delay; left > 0; {
		n := min(left, len(s.block))
		s.feed(p, s.block[:n], s.raw)
		left -= n
	}
}

func (p *dolbyPath) samples(dry []float32) []float32 {
	if p == nil || p.effect == nil {
		return dry
	}
	return p.ready
}

func (p *dolbyPath) rendersBed() bool {
	return p != nil && p.effect != nil && p.channels > 2
}

func (p *dolbyPath) consume(n int) {
	if p != nil && p.effect != nil {
		p.ready = p.ready[:copy(p.ready, p.ready[n:])]
	}
}

func (p *dolbyPath) close() {
	if p != nil && p.effect != nil {
		p.effect.close()
		p.effect = nil
	}
}

func (s *dolbyStream) SetPosition(frame int64) error {
	if err := s.stream.SetPosition(frame); err != nil {
		return err
	}
	s.next.close()
	s.next = nil
	if p := s.current; p != nil {
		if p.profile != s.requested.Load() || p.effect != nil && !p.effect.reset() {
			p.close()
			s.current = nil
		} else {
			p.ready, p.skip = p.ready[:0], p.delay
		}
	}
	s.dry, s.surround, s.end = s.dry[:0], s.surround[:0], nil
	s.demucsWeights = s.demucsWeights[:0]
	if s.clarity != nil {
		s.clarity.reset()
	}
	return nil
}

func (s *dolbyStream) Close() error {
	s.next.close()
	s.current.close()
	return s.stream.Close()
}
