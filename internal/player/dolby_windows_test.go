//go:build windows && amd64

package player

import (
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The installed APO must change PCM through the playback decoder while preserving its length.
func TestInstalledDolbyProcessesPCM(t *testing.T) {
	if os.Getenv("MM_TEST_DOLBY") == "" {
		t.Skip("requires an installed Dolby DAX3 APO")
	}
	dry := make([]float32, 48000*2)
	for i := 0; i < len(dry); i += 2 {
		dry[i] = 0.08 * float32(math.Sin(2*math.Pi*440*float64(i/2)/48000))
		dry[i+1] = 0.04 * float32(math.Sin(2*math.Pi*997*float64(i/2)/48000))
	}
	active := false
	s := &dolbyStream{stream: &dolbyTestStream{samples: dry}, status: func(_ *DolbyProfile, ok bool, _ int) { active = ok }}
	s.requested.Store(&DolbyProfile{Leveler: 2, Target: -24})
	defer s.Close()
	for _, seek := range []int64{0, 12000} {
		if err := s.SetPosition(seek); err != nil {
			t.Fatal(err)
		}
		var wet []float32
		var buf [4096]float32
		for {
			n, err := s.Read(buf[:])
			wet = append(wet, buf[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil || n == 0 {
				t.Fatalf("seek %d: Read = %d, %v", seek, n, err)
			}
		}
		if !active || len(wet) != len(dry)-int(seek)*2 {
			t.Fatalf("seek %d: APO active = %t, got %d samples", seek, active, len(wet))
		}
		checkDolbyModule(t, s.current.effect, dolbyAPOPath())
		var inputPower, outputPower float64
		for i := len(wet) / 2; i < len(wet); i++ {
			in, out := float64(dry[int(seek)*2+i]), float64(wet[i])
			inputPower += in * in
			outputPower += out * out
		}
		ratio := math.Sqrt(outputPower / inputPower)
		if math.IsNaN(ratio) || ratio <= 1.1 || ratio > 10 {
			t.Errorf("seek %d: expected a level change, got PCM RMS ratio %v", seek, ratio)
		} else {
			t.Logf("seek %d: PCM RMS ratio %.2f", seek, ratio)
		}
	}
}

// Applying new APO settings at a seek changes the PCM and returning to off restores dry audio.
func TestInstalledDolbyProfilesChangePCM(t *testing.T) {
	if os.Getenv("MM_TEST_DOLBY") == "" {
		t.Skip("requires an installed Dolby DAX3 APO")
	}
	r := rand.New(rand.NewPCG(1, 2))
	dry := make([]float32, 48000*2)
	for i := range dry {
		dry[i] = 0.1 * (2*r.Float32() - 1)
	}
	active := false
	s := &dolbyStream{stream: &dolbyTestStream{samples: dry}, status: func(_ *DolbyProfile, ok bool, _ int) { active = ok }}
	defer s.Close()
	render := func(profile DolbyProfile) []float32 {
		t.Helper()
		s.requested.Store(&profile)
		if err := s.SetPosition(0); err != nil {
			t.Fatal(err)
		}
		var got []float32
		var buf [4096]float32
		for {
			n, err := s.Read(buf[:])
			got = append(got, buf[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil || n == 0 {
				t.Fatalf("Read = %d, %v", n, err)
			}
		}
		if len(got) != len(dry) || active != profile.enabled() {
			t.Fatalf("profile %+v: APO active = %t, samples = %d", profile, active, len(got))
		}
		return got
	}
	energy := func(pcm []float32) float64 {
		var sum float64
		for _, x := range pcm[len(pcm)/2:] {
			sum += float64(x) * float64(x)
		}
		return sum
	}
	balanced := render(DolbyProfile{Leveler: 2, Target: -24})
	night := render(DolbyProfile{Leveler: 6, Target: -32})
	if ratio := math.Sqrt(energy(night) / energy(balanced)); !(ratio > 0 && ratio < 0.8) {
		t.Errorf("night effect did not lower the level: RMS ratio %.3f", ratio)
	}
	for _, effect := range []struct {
		name    string
		profile DolbyProfile
	}{
		{"dynamic", DolbyProfile{Leveler: 4, Target: -24, Dialogue: 5, IntelligentEQ: 10}},
		{"music intelligent EQ", DolbyProfile{IntelligentEQ: 8}},
		{"movie", DolbyProfile{Leveler: 4, Target: -26, Dialogue: 8}},
		{"game", DolbyProfile{Leveler: 1, Target: -28, Dialogue: 3}},
		{"dialogue enhancement", DolbyProfile{Dialogue: 12}},
	} {
		wet := render(effect.profile)
		var difference float64
		for i := len(wet) / 2; i < len(wet); i++ {
			d := float64(wet[i] - dry[i])
			difference += d * d
		}
		if diff := math.Sqrt(difference / energy(dry)); !(diff > 0.02) {
			t.Errorf("%s did not change the signal: normalized difference %.3f", effect.name, diff)
		}
	}
	balancedEQ := render(DolbyProfile{IntelligentEQ: 16})
	for _, curve := range []string{"detailed", "warm"} {
		wet := render(DolbyProfile{IntelligentEQ: 16, IntelligentEQCurve: curve})
		var difference float64
		for i := len(wet) / 2; i < len(wet); i++ {
			d := float64(wet[i] - balancedEQ[i])
			difference += d * d
		}
		if diff := math.Sqrt(difference / energy(balancedEQ)); !(diff > 0.01 && !math.IsInf(diff, 0)) {
			t.Errorf("%s intelligent EQ curve did not change the PCM: normalized difference %.4f", curve, diff)
		}
	}
	for _, check := range []struct {
		name      string
		base, set DolbyProfile
	}{
		{"input loudness target", DolbyProfile{Leveler: 6, Target: -24}, DolbyProfile{Leveler: 6, Target: -24, InputTarget: -32}},
		{"output gain", DolbyProfile{}, DolbyProfile{OutputGain: -6}},
		{"content-adaptive EQ", DolbyProfile{IntelligentEQ: 10}, DolbyProfile{IntelligentEQ: 10, IntelligentEQSteering: true}},
		{"volume modeler", DolbyProfile{Spatial: true}, DolbyProfile{Spatial: true, VolumeModeler: true, VolumeCalibration: -20}},
		{"volume modeler calibration", DolbyProfile{VolumeModeler: true}, DolbyProfile{VolumeModeler: true, VolumeCalibration: -20}},
		{"surround decoder bypass", DolbyProfile{Spatial: true}, DolbyProfile{Spatial: true, DisableSurroundDecoder: true}},
	} {
		a, b := render(check.base), render(check.set)
		var difference float64
		for i := len(a) / 2; i < len(a); i++ {
			d := float64(b[i] - a[i])
			difference += d * d
		}
		if diff := math.Sqrt(difference / energy(a)); !(diff > 0.04 && !math.IsInf(diff, 0)) {
			t.Errorf("%s did not change the PCM: normalized difference %.4f", check.name, diff)
		}
		if check.name == "output gain" {
			if ratio := math.Sqrt(energy(b) / energy(a)); !(ratio > 0.48 && ratio < 0.52) {
				t.Errorf("−6 dB output gain gave RMS ratio %.4f", ratio)
			}
		}
	}
	if got := render(DolbyProfile{}); !slices.Equal(got, dry) {
		t.Error("turning off the effect did not restore the original PCM")
	}
}

// The requested package's APO renders a left-only input into both headphone channels.
func TestPackageDolbySpatialRendersBothEars(t *testing.T) {
	if os.Getenv("MM_TEST_DOLBY") == "" {
		t.Skip("requires the AAF Dolby DAX3 APO package")
	}
	proc := dolbyClassObject()
	var module windows.Handle
	path, err := windows.UTF16PtrFromString(dolbyAPOPath())
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, path, &module); err != nil {
		t.Fatal(err)
	}
	if expected, err := windows.GetProcAddress(module, "DllGetClassObject"); err != nil || expected != proc {
		t.Fatalf("APO factory is not from the AAF package: %v", err)
	}
	random := rand.New(rand.NewPCG(3, 5))
	for _, rate := range []int{44100, 48000, 96000} {
		effect, _ := openDolbyEffect(rate, 2, DolbyProfile{Spatial: true})
		if effect == nil {
			t.Fatalf("%d Hz: failed to open AAF APO", rate)
		}
		checkDolbyModule(t, effect, dolbyAPOPath())
		var energy [2]float64
		var samples [1024]float32
		for range 50 {
			for i := 0; i < len(samples); i += 2 {
				samples[i], samples[i+1] = 0.05*(2*random.Float32()-1), 0
			}
			if !effect.process(samples[:]) {
				effect.close()
				t.Fatalf("%d Hz: AAF APO did not return spatial PCM", rate)
			}
			for i, x := range samples {
				energy[i%2] += float64(x) * float64(x)
			}
		}
		effect.close()
		if ratio := math.Sqrt(energy[1] / energy[0]); !(ratio > 0.05 && ratio < 0.99) {
			t.Errorf("%d Hz: expected binaural crossfeed, right/left RMS ratio %.3f", rate, ratio)
		}
	}
}

func checkDolbyModule(t *testing.T, effect dolbyEffect, path string) {
	t.Helper()
	pathPtr, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	const unchanged = windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT
	var want, got windows.Handle
	if err := windows.GetModuleHandleEx(unchanged, pathPtr, &want); err != nil {
		t.Fatalf("loading APO module %s: %v", path, err)
	}
	d := effect.(*windowsDolby)
	vtable := *(*unsafe.Pointer)(d.realtime)
	process := *(**uint16)(unsafe.Add(vtable, 3*unsafe.Sizeof(uintptr(0))))
	if err := windows.GetModuleHandleEx(unchanged|windows.GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS, process, &got); err != nil || got != want {
		t.Fatalf("APOProcess did not come from %s: %v", path, err)
	}
}

// The AAF headphone APO produces different stereo responses for front and height signals.
func TestAAFAtmosHeightChangesPCM(t *testing.T) {
	if os.Getenv("MM_TEST_DOLBY") == "" {
		t.Skip("requires the AAF APO")
	}
	render := func(channel int) []float32 {
		t.Helper()
		effect, _ := openDolbyEffect(codecRate, atmosPCMChannels, DolbyProfile{Spatial: true})
		if effect == nil {
			t.Fatal("AAF APO did not open the 7.1.2 format")
		}
		defer effect.close()
		var in [512 * atmosPCMChannels]float32
		var out [1024]float32
		var pcm []float32
		rng := rand.New(rand.NewPCG(11, 13))
		for range 32 {
			for i := channel; i < len(in); i += atmosPCMChannels {
				in[i] = 0.08 * (2*rng.Float32() - 1)
			}
			if !effect.(dolbySurroundEffect).render(in[:], out[:]) {
				t.Fatal("AAF APO did not produce stereo PCM")
			}
			pcm = append(pcm, out[:]...)
		}
		return pcm
	}
	front, height := render(0), render(8)
	var energy, difference float64
	for i, sample := range height {
		v := float64(sample)
		diff := v - float64(front[i])
		energy += v * v
		difference += diff * diff
	}
	if !(energy > 0.001 && difference/energy > 0.01 && !math.IsInf(difference, 0)) {
		t.Fatalf("height output energy %g, difference from front %g", energy, difference)
	}
}

// The AAF add-ons must affect their target frequencies and backing audio with a preset active.
func TestAAFDolbyAddOnsWithPresets(t *testing.T) {
	if os.Getenv("MM_TEST_DOLBY") == "" {
		t.Skip("requires the AAF APO")
	}
	const rate = 48000
	for _, preset := range []struct {
		name     string
		channels int
		profile  DolbyProfile
	}{
		{"Music", 2, DolbyProfile{IntelligentEQ: 8}},
		{"Headphones", atmosPCMChannels, DolbyProfile{Spatial: true}},
	} {
		t.Run(preset.name, func(t *testing.T) {
			render := func(t *testing.T, profile DolbyProfile, input []float32) []float32 {
				t.Helper()
				effect, delay := openDolbyEffect(rate, preset.channels, profile)
				if effect == nil {
					t.Fatal("could not open the AAF APO")
				}
				defer effect.close()
				out := make([]float32, len(input)/preset.channels*2)
				for start := 0; start < len(out)/2; start += apoBlockFrames {
					end := min(start+apoBlockFrames, len(out)/2)
					if !effect.(dolbySurroundEffect).render(input[start*preset.channels:end*preset.channels], out[start*2:end*2]) {
						t.Fatal("AAF APO did not produce stereo PCM")
					}
				}
				return out[delay:]
			}
			sine := func(frame int, hz float64) float32 {
				return float32(math.Sin(2 * math.Pi * hz * float64(frame) / rate))
			}
			tonePower := func(wet []float32, hz float64, start, frames int) float64 {
				var re, im [2]float64
				for i := start; i < start+frames; i++ {
					s, c := math.Sincos(2 * math.Pi * hz * float64(i) / rate)
					for ch := range 2 {
						x := float64(wet[i*2+ch])
						re[ch] += x * c
						im[ch] += x * s
					}
				}
				return re[0]*re[0] + im[0]*im[0] + re[1]*re[1] + im[1]*im[1]
			}
			const bassHz, eqHz, referenceHz = 65, 1031, 3750
			tones := make([]float32, rate*preset.channels)
			for i := range rate {
				l, r := 0.025*sine(i, bassHz), 0.025*sine(i, bassHz)
				l, r = l+0.011*sine(i, eqHz), r-0.011*sine(i, eqHz)
				off := i * preset.channels
				if preset.channels == atmosPCMChannels {
					tones[off+8] = 0.01 * sine(i, referenceHz) // height channel
				} else {
					l, r = l+0.01*sine(i, referenceHz), r+0.01*sine(i, referenceHz)
				}
				tones[off], tones[off+1] = l, r
			}
			bandRatio := func(wet []float32, hz float64) float64 {
				return tonePower(wet, hz, rate/4, rate/2) / tonePower(wet, referenceHz, rate/4, rate/2)
			}
			profile := preset.profile
			base := render(t, profile, tones)
			profile.BassBoost = 12
			bass := render(t, profile, tones)
			if db := 10 * math.Log10(bandRatio(bass, bassHz)/bandRatio(base, bassHz)); !(db > 2 && db < 20) {
				t.Errorf("bass boost changed low tones by %.2f dB", db)
			} else {
				t.Logf("bass boost changed low tones by %.2f dB", db)
			}
			profile.GraphicEQ = true
			profile.GraphicGains[7] = -12
			graphic := render(t, profile, tones)
			if db := 10 * math.Log10(bandRatio(graphic, eqHz)/bandRatio(bass, eqHz)); !(db < -3 && db > -24) {
				t.Errorf("graphic EQ changed its target tone by %.2f dB", db)
			} else {
				t.Logf("graphic EQ changed its target tone by %.2f dB", db)
			}
			graphicOnly := profile
			graphicOnly.BassBoost = 0
			if db := 10 * math.Log10(bandRatio(render(t, graphicOnly, tones), eqHz)/bandRatio(base, eqHz)); !(db < -3 && db > -24) {
				t.Errorf("graphic EQ without bass enhancement changed its target tone by %.2f dB", db)
			}
			if profile.IntelligentEQ > 0 {
				withoutIEQ := profile
				withoutIEQ.IntelligentEQ = 0
				if db := 10 * math.Log10(bandRatio(graphic, eqHz)/bandRatio(render(t, withoutIEQ, tones), eqHz)); !(math.Abs(db) > 0.2 && !math.IsInf(db, 0)) {
					t.Errorf("intelligent EQ changed its relative tone by %.2f dB with add-ons active", db)
				}
			}
			if preset.channels == atmosPCMChannels && tonePower(graphic, referenceHz, rate/4, rate/2) < 1 {
				t.Error("headphone rendering lost the height channel with add-ons active")
			}
			t.Run("Ducking", func(t *testing.T) {
				path := os.Getenv("MM_TEST_DOLBY_SPEECH")
				if path == "" {
					t.Skip("set MM_TEST_DOLBY_SPEECH to a dry mono speech WAV")
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				wav, err := openWAVStream(downloaded(data))
				if err != nil {
					t.Fatal(err)
				}
				defer wav.Close()
				speech := make([]float32, 2*int(wav.Len()))
				if n, err := wav.Read(speech); n != len(speech) || err != nil {
					t.Fatalf("read speech WAV: %d samples, %v", n, err)
				}
				const frames, backingHz = 7 * rate, 2500
				input := make([]float32, frames*preset.channels)
				for i := range frames {
					at := float64(i) * float64(wav.SampleRate()) / rate
					j := int(at)
					var voice float32
					if j+1 < len(speech)/2 {
						a, b := speech[j*2], speech[(j+1)*2]
						voice = 0.35 * (a + (b-a)*float32(at-float64(j)))
					}
					off := i * preset.channels
					input[off] = 0.018*sine(i, 469) + 0.015*sine(i, backingHz)
					input[off+1] = 0.018*sine(i, 844) - 0.015*sine(i, backingHz)
					if preset.channels > 2 {
						input[off+2] = voice
					} else {
						input[off] += voice
						input[off+1] += voice
					}
				}
				// Compare at the same dialogue strength to isolate ducking from enhancement.
				enhanced := profile
				enhanced.Dialogue = 5
				base := render(t, enhanced, input)
				profile.DialogueDucking = 16 // Also exercises automatic dialogue enhancement.
				ducked := render(t, profile, input)
				db := 10 * math.Log10(tonePower(ducked, backingHz, rate/4, 5*rate)/tonePower(base, backingHz, rate/4, 5*rate))
				if !(db < -0.2 && db > -24) {
					t.Errorf("ducker changed the backing tone by %.2f dB", db)
				} else {
					t.Logf("ducker changed the backing tone by %.2f dB", db)
				}
			})
		})
	}
}

// Switching a track's APO reports the applied path without reopening playback.
func TestInstalledDolbyThroughQueue(t *testing.T) {
	if os.Getenv("MM_TEST_DOLBY") == "" {
		t.Skip("requires an installed Dolby DAX3 APO")
	}
	p, posts, src, queue := servedQueue(t, "a")
	profile := DolbyProfile{Leveler: 2, Target: -24}
	p.Dolby = func() DolbyProfile { return profile }
	p.PlayQueue(queue, 0)
	waitApplied := func(wantActive bool) {
		deadline := time.After(2 * time.Second)
		for {
			state := p.DolbyProcessing()
			if !state.Pending && state.Profile == profile && state.InputChannels > 0 {
				if state.Active != wantActive || state.SampleRate != 44100 || state.InputChannels != 2 || !p.Playing() || p.Err() != "" {
					t.Fatalf("unexpected processing state %+v, playing = %t, error = %q", state, p.Playing(), p.Err())
				}
				return
			}
			select {
			case fn := <-posts:
				fn()
			case <-deadline:
				t.Fatalf("playback did not start Dolby: %q", p.Err())
			}
		}
	}
	waitApplied(true)
	if _, ok := p.stream.(*mp3Stream); !ok {
		t.Fatal("playback did not use the original decoder")
	}
	out := p.out
	previous := profile
	profile = DolbyProfile{Spatial: true}
	p.ApplyDolby()
	if p.seeking {
		t.Fatal("switching the effect sought the track")
	}
	if state := p.DolbyProcessing(); !state.Pending || !state.Active || state.Profile != previous {
		t.Fatalf("switch discarded the applied profile before the decoder committed: %+v", state)
	}
	waitApplied(true)
	profile = DolbyProfile{}
	p.ApplyDolby()
	waitApplied(false)
	src.mu.Lock()
	fetches := src.fetched["/a"]
	src.mu.Unlock()
	if p.out != out || fetches != 1 {
		t.Fatalf("switching the effect reopened playback (fetches %d)", fetches)
	}
}
