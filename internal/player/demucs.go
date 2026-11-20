package player

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"math"
	"sync/atomic"
	"time"
	"unsafe"
)

// DemucsMode selects the inference device for new stereo tracks.
type DemucsMode string

const (
	DemucsOff  DemucsMode = ""
	DemucsAuto DemucsMode = "auto"
	DemucsCUDA DemucsMode = "cuda"
	DemucsCPU  DemucsMode = "cpu"
)

type DemucsStatus struct {
	Device    DemucsMode
	Requested bool
	Active    bool
	Failed    bool
	Speed     float64 // processed audio seconds per wall-clock second
}

// DemucsMix adjusts the gain of each stem in dB.
type DemucsMix struct {
	Vocals float64 `json:"vocals,omitempty"`
	Drums  float64 `json:"drums,omitempty"`
	Bass   float64 `json:"bass,omitempty"`
	Other  float64 `json:"other,omitempty"`
}

const (
	DemucsMinGain      = -12
	DemucsMaxGain      = 6
	demucsFrameSamples = atmosPCMChannels + 8
)

const (
	demucsVocals = iota
	demucsDrums
	demucsBass
	demucsOther
	demucsStemCount
)

// Offsets from unity make the zero value the original mix.
type demucsGains [demucsStemCount]float32

type demucsWorker struct {
	device DemucsMode
	rhythm bool
	input  io.Writer
	output io.Reader
	Close  func()
	abort  func()
}

func (w *demucsWorker) render(rate int, stereo []float32) ([]float32, error) {
	if w.abort != nil {
		defer time.AfterFunc(10*time.Second, w.abort).Stop()
	}
	frames := len(stereo) / 2
	if err := w.writePCM(uint32(frames), rate, stereo); err != nil {
		return nil, err
	}
	bed := make([]float32, frames*demucsFrameSamples)
	_, err := io.ReadFull(w.output, unsafe.Slice((*byte)(unsafe.Pointer(&bed[0])), 4*len(bed)))
	return bed, err
}

func (w *demucsWorker) writePCM(frames uint32, rate int, stereo []float32) error {
	var header [8]byte
	binary.LittleEndian.PutUint32(header[:], frames)
	binary.LittleEndian.PutUint32(header[4:], uint32(rate))
	if _, err := w.input.Write(header[:]); err != nil {
		return err
	}
	pcm := unsafe.Slice((*byte)(unsafe.Pointer(&stereo[0])), 4*len(stereo))
	_, err := w.input.Write(pcm)
	return err
}

type demucsRequest struct{ frame int64 }

type demucsBlock struct {
	request   *demucsRequest
	start     int64
	bed       []float32
	stats     *demucsStats
	placement demucsPlacement
}

func (b *demucsBlock) end() int64 {
	return b.start + int64(len(b.bed)/demucsFrameSamples)
}

type demucsStream struct {
	stream
	engine      *demucsEngine
	cancel      context.CancelFunc
	request     atomic.Pointer[demucsRequest]
	position    atomic.Int64
	wake        chan struct{}
	results     chan demucsBlock
	processing  atomic.Pointer[demucsStats]
	failed      atomic.Bool
	wantedMix   atomic.Pointer[demucsGains]
	positioning atomic.Bool
	rhythm      atomic.Pointer[beatTimeline]

	// Only the playback decoder touches these fields.
	current, next                             demucsBlock
	mix                                       float32
	gains, target                             demucsGains
	gainStep                                  demucsGains
	gainFrames                                int
	placement, placementTarget, placementStep demucsPlacement
	placementFrames                           int
}

func newDemucsStream(base stream, engine *demucsEngine, duration int64) *demucsStream {
	ctx, cancel := context.WithCancel(context.Background())
	s := &demucsStream{stream: base, engine: engine, cancel: cancel, wake: make(chan struct{}, 1), results: make(chan demucsBlock, 2)}
	s.request.Store(&demucsRequest{})
	engine.retain()
	go s.analyzeRhythm(ctx, duration)
	go s.produce(ctx, duration)
	return s
}

func (s *demucsStream) surroundChannels() int { return atmosPCMChannels }

func (p *Player) applyDemucsMix() {
	s, ok := p.stream.(*demucsStream)
	if !ok {
		return
	}
	s.positioning.Store(p.DemucsPositioning != nil && p.DemucsPositioning())
	gains := p.demucsGains()
	if previous := s.wantedMix.Load(); previous == nil || *previous != gains {
		s.wantedMix.Store(&gains)
	}
}

func (p *Player) demucsGains() demucsGains {
	if p.DemucsMix == nil {
		return demucsGains{}
	}
	mix := p.DemucsMix()
	offset := func(db float64) float32 {
		return float32(math.Pow(10, min(DemucsMaxGain, max(DemucsMinGain, db))/20) - 1)
	}
	return demucsGains{
		demucsVocals: offset(mix.Vocals),
		demucsDrums:  offset(mix.Drums),
		demucsBass:   offset(mix.Bass),
		demucsOther:  offset(mix.Other),
	}
}

func (s *demucsStream) readSurround(stereo, bed, weights []float32) (int, error) {
	n, err := s.stream.Read(stereo)
	frames := n / 2
	position := s.position.Load()
	request := s.request.Load()
	positioning := s.positioning.Load()
	fade := max(1, s.SampleRate()*dolbyFadeMillis/1000)
	delta := 1 / float32(fade)
	if target := s.wantedMix.Load(); target != nil && *target != s.target {
		s.target, s.gainFrames = *target, fade
		for stem := range s.target {
			s.gainStep[stem] = (s.target[stem] - s.gains[stem]) * delta
		}
	}
	var used *demucsStats
	for first := 0; first < frames; {
		at := position + int64(first)
		for s.current.request != request || s.current.end() <= at {
			s.current, s.next = s.next, demucsBlock{}
			if s.current.request == nil {
				s.current = s.takeBlock(request, at)
			}
			if s.current.request == nil {
				break
			}
		}
		if s.next.request == nil {
			s.next = s.takeBlock(request, at)
		}
		last := frames
		wet := s.current.request != nil && s.current.start <= at
		if wet {
			last = min(last, first+int(s.current.end()-at))
		} else if s.current.request != nil {
			last = min(last, first+int(s.current.start-at))
		}
		if !wet {
			s.gains, s.gainFrames = s.target, 0
		} else {
			var placement demucsPlacement
			if positioning {
				placement = s.current.placement
			}
			if s.mix == 0 {
				s.placement, s.placementTarget, s.placementFrames = placement, placement, 0
			} else if placement != s.placementTarget {
				s.placementTarget = placement
				s.placementFrames = max(1, s.SampleRate()*placementFadeMillis/1000)
				for i := range placement {
					s.placementStep[i] = (placement[i] - s.placement[i]) / float32(s.placementFrames)
				}
			}
		}
		applyPlacement := wet && (s.placementFrames != 0 || s.placement != (demucsPlacement{}))
		for i := first; i < last; i++ {
			dst := bed[i*atmosPCMChannels : (i+1)*atmosPCMChannels]
			clear(dst)
			dst[0], dst[1] = stereo[i*2], stereo[i*2+1]
			if !wet {
				s.mix = 0
				weights[i] = 0
				continue
			}
			s.mix = min(1, s.mix+delta)
			frame := position + int64(i)
			if s.next.request == nil || s.next.start != s.current.end() {
				s.mix = min(s.mix, float32(s.current.end()-frame-1)*delta)
			}
			weight := s.mix * s.mix * (3 - 2*s.mix)
			if weight > 0 {
				used = s.current.stats
			}
			src := s.current.bed[(frame-s.current.start)*demucsFrameSamples:]
			weights[i] = weight
			if s.gainFrames > 0 {
				s.gainFrames--
				if s.gainFrames > 0 {
					for stem := range s.gains {
						s.gains[stem] += s.gainStep[stem]
					}
				} else {
					s.gains = s.target
				}
			}
			mixDemucsFrame(dst, src, weight, s.gains)
			if s.gains != (demucsGains{}) {
				mixDemucsStereo(stereo[i*2:i*2+2], src, weight, s.gains)
			}
			if applyPlacement {
				if s.placementFrames > 0 {
					s.placementFrames--
					if s.placementFrames == 0 {
						s.placement = s.placementTarget
					} else {
						for j := range s.placement {
							s.placement[j] += s.placementStep[j]
						}
					}
				}
				addDemucsPlacement(dst, src, weight, s.gains, &s.placement)
			}
		}
		first = last
	}
	if frames > 0 {
		s.setProcessing(used)
	}
	s.position.Store(position + int64(frames))
	return n, err
}

func mixDemucsFrame(dst, src []float32, wet float32, gains demucsGains) {
	// The original mix includes the separation residual. Keep it with the other instruments.
	other := gains[demucsOther]
	for ch := range dst {
		dst[ch] += wet * ((1+other)*src[ch] - dst[ch])
	}
	voice := wet * (gains[demucsVocals] - other)
	drums := wet * (gains[demucsDrums] - other)
	bass := wet * (gains[demucsBass] - other)
	if voice == 0 && drums == 0 && bass == 0 {
		return
	}
	center := voice * src[2]
	dst[2] += center
	for ch := range 2 {
		rear, height := drums*src[4+ch], drums*src[16+ch]
		dst[4+ch] += rear
		dst[8+ch] += height
		// Each adjusted stem keeps its existing sends and their compensation in the front pair.
		dst[ch] += voice*src[10+ch] + drums*src[12+ch] + bass*src[14+ch] - demucsFoldGain*(center+rear+height)
	}
}

func mixDemucsStereo(dst, src []float32, wet float32, gains demucsGains) {
	other := gains[demucsOther]
	for ch := range 2 {
		// Keep the separation residual with Other and leave unity gains sample-identical.
		delta := other*dst[ch] + (gains[demucsVocals]-other)*src[10+ch] +
			(gains[demucsDrums]-other)*src[12+ch] + (gains[demucsBass]-other)*src[14+ch]
		dst[ch] += wet * delta
	}
}

func (s *demucsStream) takeBlock(request *demucsRequest, at int64) demucsBlock {
	for {
		select {
		case b := <-s.results:
			if b.request == request && b.end() > at {
				return b
			}
		default:
			return demucsBlock{}
		}
	}
}

func (s *demucsStream) SetPosition(frame int64) error {
	if err := s.stream.SetPosition(frame); err != nil {
		return err
	}
	s.current, s.next, s.mix = demucsBlock{}, demucsBlock{}, 0
	s.setProcessing(nil)
	s.position.Store(frame)
	s.request.Store(&demucsRequest{frame: frame})
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *demucsStream) Close() error {
	s.cancel()
	return s.stream.Close()
}

func (s *demucsStream) setProcessing(stats *demucsStats) {
	if s.processing.Load() == stats {
		return
	}
	previous := s.processing.Swap(stats)
	if s.engine != nil && (previous == nil || stats == nil || previous.device != stats.device) {
		s.engine.changed()
	}
}

func (s *demucsStream) produce(ctx context.Context, duration int64) {
	defer s.engine.release()
	rate := s.SampleRate()
	dl, _ := s.source()
	decoder, err := openShared(dl, duration)
	if err != nil {
		s.demucsError(ctx, err)
		return
	}
	defer decoder.Close()

	step, overlap := 2*rate, rate/4
	input := make([]float32, 2*(step+overlap))
	tail := make([]float32, overlap*demucsFrameSamples)
	var request *demucsRequest
	var cursor int64
	eof, retained := false, 0
	for ctx.Err() == nil {
		want := s.request.Load()
		position := s.position.Load()
		if request != want || !eof && cursor+int64(step) <= position {
			start := max(want.frame, position)
			if request != nil || start != 0 {
				if err := decoder.SetPosition(start); err != nil {
					s.demucsError(ctx, err)
					return
				}
			}
			request, cursor, eof, retained = want, start, false, 0
		}
		if eof {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		count := retained * 2
		for count < len(input) {
			n, readErr := decoder.Read(input[count:])
			count += n
			if readErr != nil && readErr != io.EOF {
				s.demucsError(ctx, readErr)
				return
			}
			if n == 0 || readErr == io.EOF {
				eof = true
				break
			}
		}
		frames := count / 2
		if frames == 0 {
			continue
		}
		clear(input[count:])
		result := s.engine.render(ctx, rate, input)
		if result.err != nil {
			s.demucsError(ctx, result.err)
			return
		}
		pcm := result.bed
		for i := range min(retained, frames) {
			t := float32(i) / float32(overlap)
			for ch := range demucsFrameSamples {
				j := i*demucsFrameSamples + ch
				pcm[j] = tail[j] + t*(pcm[j]-tail[j])
			}
		}
		if !eof {
			copy(tail, pcm[step*demucsFrameSamples:])
			copy(input, input[step*2:])
			retained = overlap
			frames = step
		}
		b := demucsBlock{request: request, start: cursor, bed: pcm[:frames*demucsFrameSamples], stats: result.stats}
		b.placement = analyzeDemucsPlacement(b.bed)
		cursor += int64(frames)
		for queued := false; !queued && s.request.Load() == request; {
			select {
			case <-ctx.Done():
				return
			case s.results <- b:
				queued = true
			case <-s.wake:
			}
		}
	}
}

func (s *demucsStream) demucsError(ctx context.Context, err error) {
	if ctx.Err() == nil {
		s.failed.Store(true)
		s.engine.changed()
		log.Printf("Demucs: %v", err)
	}
}
