//go:build windows && amd64

package player

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The Demucs route applies stem gains in stereo PCM before the non-spatial AAF APO.
func TestNativeDemucsDolbyDirect(t *testing.T) {
	mode := DemucsMode(os.Getenv("MM_DEMUCS_NATIVE"))
	if os.Getenv("MM_TEST_DOLBY") == "" || mode == DemucsOff {
		t.Skip("requires the native Demucs worker, codec and AAF APO")
	}
	worker := findDemucs()
	if worker == "" {
		t.Fatal("Demucs worker or model missing")
	}
	const inputRate = codecRate
	var raw bytes.Buffer
	for i := range inputRate {
		x := float32(0.2 * math.Sin(2*math.Pi*440*float64(i)/inputRate))
		binary.Write(&raw, binary.LittleEndian, [2]float32{x, 0})
	}
	dl := downloaded(wavFile(wavFloat, 2, 32, inputRate, raw.Bytes()))
	base, err := openTrackStream(t.Context(), dl, 0)
	if err != nil {
		dl.Close()
		t.Fatal(err)
	}
	engine := newDemucsEngine(t.Context(), mode, func(ctx context.Context, mode DemucsMode) (*demucsWorker, error) {
		return startDemucs(ctx, worker, mode, codecRate)
	}, func() {})
	s := newDemucsStream(base, engine, 1000)
	defer s.Close()
	gains := demucsGains{-0.5, -0.5, -0.5, -0.5}
	s.wantedMix.Store(&gains)
	select {
	case block := <-s.results:
		s.results <- block
	case <-t.Context().Done():
		t.Fatal("the producer did not queue stem audio")
	}
	var bed [1024 * atmosPCMChannels]float32
	var weights [1024]float32
	var buf [2048]float32
	if _, err := s.readSurround(buf[:], bed[:], weights[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readSurround(buf[:], bed[:], weights[:]); err != nil {
		t.Fatal(err)
	}
	n, err := s.readSurround(buf[:], bed[:], weights[:])
	if err != nil || n != len(buf) || s.processing.Load() == nil {
		t.Fatalf("stem mix read %d samples: %v", n, err)
	}
	var peak [2]float64
	for i, sample := range buf[n/2 : n] {
		peak[i%2] = max(peak[i%2], math.Abs(float64(sample)))
	}
	if !(peak[0] > 0.08 && peak[0] < 0.12 && peak[1] < 0.002) {
		t.Fatalf("stem mix has channel peaks %v", peak)
	}
	profile := DolbyProfile{IntelligentEQ: 8}
	active := false
	d := &dolbyStream{stream: s, status: func(_ *DolbyProfile, ok bool, channels int) { active = ok && channels == 2 }}
	d.requested.Store(&profile)
	defer d.Close()
	if n, err := d.Read(buf[:]); err != nil || n == 0 || !active {
		t.Fatalf("the Dolby APO did not process the decoded stereo: %d samples, active = %t, err = %v", n, active, err)
	}
	checkDolbyModule(t, d.current.effect, dolbyAPOPath())
}

// A rear-only channel decoded through the codec bridge reaches both ears through the AAF APO with its 5.1 input intact.
func TestNativeDolbySurroundRendersBothEars(t *testing.T) {
	if os.Getenv("MM_TEST_DOLBY") == "" {
		t.Skip("requires the MyGo native codec and AAF APO")
	}
	b := make([]byte, codecRate*6*4)
	for i := range codecRate {
		sample := 0.1 * float32(math.Sin(2*math.Pi*997*float64(i)/codecRate))
		binary.LittleEndian.PutUint32(b[(6*i+4)*4:], math.Float32bits(sample))
	}
	dl := downloaded(wavFile(wavFloat, 6, 32, codecRate, b))
	defer dl.Close()
	s, err := openCodecPCM(t.Context(), dl, "wav")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	decoded, ok := s.(*codecPCM)
	if !ok || !decoded.downmix {
		t.Fatal("decoder did not preserve the 5.1 PCM")
	}
	if err := s.SetPosition(codecRate / 2); err != nil {
		t.Fatal(err)
	}
	var samples [1024]float32
	var surround [3072]float32
	n, err := decoded.read(samples[:], surround[:])
	if err != nil || n != len(samples) {
		t.Fatalf("reading surround PCM: %d, %v", n, err)
	}
	var rear, other float64
	for i, x := range surround {
		if i%6 == 4 {
			rear += float64(x) * float64(x)
		} else {
			other += float64(x) * float64(x)
		}
	}
	if !(rear > 0.001 && rear > 100*other) {
		t.Fatalf("rear-left signal was moved into another channel: %g vs %g", rear, other)
	}
	if err := s.SetPosition(0); err != nil {
		t.Fatal(err)
	}
	active := false
	spatial := &dolbyStream{stream: s, status: func(_ *DolbyProfile, ok bool, _ int) { active = ok }}
	spatial.requested.Store(&DolbyProfile{Spatial: true})
	defer spatial.Close()
	var energy [2]float64
	var frames int64
	for {
		n, err := spatial.Read(samples[:])
		frames += int64(n / 2)
		for i, sample := range samples[:n] {
			energy[i%2] += float64(sample) * float64(sample)
		}
		if err == io.EOF {
			break
		}
		if err != nil || n == 0 {
			t.Fatalf("Read = %d, %v", n, err)
		}
	}
	if !active || spatial.current.channels != 6 || frames != s.Len() {
		t.Fatalf("AAF active = %t, input channels = %d, frames = %d/%d", active, spatial.current.channels, frames, s.Len())
	}
	if !(energy[0] > 0 && energy[1] > 0 && !math.IsInf(energy[0]+energy[1], 0)) {
		t.Errorf("expected audio in both ears, got energy %v", energy)
	}
}

// Real TrueHD and Dolby Digital Plus object presentations reach the package APO as 7.1.2 PCM.
func TestNativeAtmosObjectsThroughAAF(t *testing.T) {
	dir := os.Getenv("MM_TEST_ATMOS_DIR")
	if dir == "" {
		t.Skip("requires atmos.thd and atmos.mp4 reference samples")
	}
	for _, name := range []string{"atmos.thd", "atmos.mp4"} {
		t.Run(name, func(t *testing.T) {
			dl, err := openFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			defer dl.Close()
			base, err := openTrackStream(context.Background(), dl, 0)
			if err != nil {
				t.Fatal(err)
			}
			active, channels := false, 0
			spatial := &dolbyStream{stream: base, status: func(_ *DolbyProfile, ok bool, input int) { active, channels = ok, input }}
			spatial.requested.Store(&DolbyProfile{Spatial: true})
			defer spatial.Close()
			codec, ok := base.(*codecPCM)
			if !ok || codec.format.channels != atmosPCMChannels {
				t.Fatal("the track fell back to the channel presentation")
			}
			var buf [4096]float32
			var power float64
			var frames int64
			for {
				n, err := spatial.Read(buf[:])
				frames += int64(n / 2)
				for _, x := range buf[:n] {
					power += float64(x) * float64(x)
				}
				if err == io.EOF {
					break
				}
				if err != nil || n == 0 {
					t.Fatalf("Read = %d, %v", n, err)
				}
			}
			if !active || channels != atmosPCMChannels || frames != codec.Len() || !(power > 0 && !math.IsInf(power, 0)) {
				t.Fatalf("AAF active = %t, channels = %d, processed frames = %d/%d, PCM energy = %.4g", active, channels, frames, codec.Len(), power)
			}
			t.Logf("7.1.2 object scene rendered to stereo, %d frames", frames)
		})
	}
}
