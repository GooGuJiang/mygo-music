package player

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

const (
	codecRate        = 48000
	codecWAVHeader   = 44
	atmosPCMChannels = 10 // FL, FR, FC, LFE, BL, BR, SL, SR, TFL, TFR
)

var (
	errNativeCodec = errors.New("native Dolby codec is unavailable")
	errCodecFailed = errors.New("native audio codec failed")
)

// codecPCM owns the temporary PCM and keeps the original download for progress.
type codecPCM struct {
	*wavStream
	input     *download
	dropInput func()
	once      sync.Once
}

func (s *codecPCM) source() (*download, *spectrum) { return s.input, s.spec }

func (s *codecPCM) surroundChannels() int { return s.format.channels }

func (s *codecPCM) readSurround(stereo, bed, _ []float32) (int, error) {
	return s.wavStream.read(stereo, bed)
}

func (s *codecPCM) Close() error {
	s.once.Do(func() {
		s.wavStream.Close()
		s.dropInput()
	})
	return nil
}

func openCodecPCM(ctx context.Context, dl *download, format string) (stream, error) {
	dir, err := os.MkdirTemp("", "mygo-codec-*")
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	dl.waitFor(math.MaxInt64)
	dl.mu.Lock()
	var input string
	if dl.file != nil {
		input = dl.file.Name()
	}
	err = dl.err
	dl.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if input == "" {
		input = filepath.Join(dir, "input")
		f, err := os.Create(input)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(f, &downloadReader{d: dl})
		if err = errors.Join(err, f.Close()); err != nil {
			return nil, err
		}
	}
	input, err = filepath.Abs(input)
	if err != nil {
		return nil, err
	}
	pcm := filepath.Join(dir, "decoded.wav")
	channels, err := decodeCodecPCM(ctx, input, format, pcm)
	if err != nil {
		return nil, err
	}
	if err := finishCodecWAV(pcm, channels); err != nil {
		return nil, err
	}
	decoded, err := openFile(pcm)
	if err != nil {
		return nil, err
	}
	wav, err := openWAVStream(decoded)
	if err != nil {
		decoded.Close()
		return nil, err
	}
	// Readers opened for captions may outlive the playback stream.
	decoded.tempDir = dir
	wav.downmix = channels > 2
	keep = true
	return &codecPCM{wavStream: wav, input: dl, dropInput: dl.Close}, nil
}

func finishCodecWAV(path string, channels int) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	const header = codecWAVHeader
	n := stat.Size() - header
	if (channels != 2 && channels != 6 && channels != atmosPCMChannels) || n <= 0 || n > math.MaxUint32-36 || n%int64(channels*4) != 0 {
		return errors.New("invalid decoded PCM length")
	}
	var b [header]byte
	copy(b[:4], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(n+36))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], wavFloat)
	binary.LittleEndian.PutUint16(b[22:], uint16(channels))
	binary.LittleEndian.PutUint32(b[24:], codecRate)
	binary.LittleEndian.PutUint32(b[28:], uint32(codecRate*channels*4))
	binary.LittleEndian.PutUint16(b[32:], uint16(channels*4))
	binary.LittleEndian.PutUint16(b[34:], 32)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n))
	_, err = f.WriteAt(b[:], 0)
	return err
}
