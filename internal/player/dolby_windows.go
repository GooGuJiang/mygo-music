//go:build windows && amd64

package player

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var dolbyAPOPath = sync.OnceValue(func() string {
	for _, dir := range bundledCodecDirs() {
		path := filepath.Join(dir, "DolbyAPOv251.dll")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
})

var (
	apoOle32         = windows.NewLazySystemDLL("ole32.dll")
	apoCoInitialize  = apoOle32.NewProc("CoInitializeEx")
	apoCoCreate      = apoOle32.NewProc("CoCreateInstance")
	clearAPOProperty = apoOle32.NewProc("PropVariantClear")
	apoProps         = windows.NewLazySystemDLL("propsys.dll")
	newPropertyStore = apoProps.NewProc("PSCreateMemoryPropertyStore")
	mergeAPOStores   = apoProps.NewProc("PSCreateMultiplexPropertyStore")
	daxClass         = apoGUID("{D9916FA0-99F7-11E7-BF21-6C0B849889E1}")
	daxParameters    = apoGUID("{F112024A-FE30-42A8-80AB-8DD825A06F78}")
	endpointProps    = apoGUID("{1DA5D803-D492-4EDD-8C23-E0C0FFEE7F0E}")
	classFactoryIID  = apoGUID("{00000001-0000-0000-C000-000000000046}")
	unknownIID       = apoGUID("{00000000-0000-0000-C000-000000000046}")
	apoIID           = apoGUID("{FD7F2B29-24D0-4B5C-B177-592C39F9CA10}")
	apoConfigIID     = apoGUID("{0E5ED805-ABA6-49C3-8F9A-2B8C889C4FA8}")
	apoRealtimeIID   = apoGUID("{9E1D6A6D-DDBC-4E95-A4C7-AD64BA37846C}")
	propertyStoreIID = apoGUID("{886D8EEB-8CF2-4446-8D02-CDBA1DBDCF99}")
	mediaTypeIID     = apoGUID("{4E997F73-B71F-4798-873B-ED7DFCF15B4D}")
	pcmFloatGUID     = apoGUID("{00000003-0000-0010-8000-00AA00389B71}")
	deviceEnumClass  = apoGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	deviceEnumIID    = apoGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
)

var dolbyClassObject = sync.OnceValue(func() uintptr { return loadDolbyClassObject(dolbyAPOPath()) })

func loadDolbyClassObject(path string) uintptr {
	if !filepath.IsAbs(path) {
		return 0
	}
	h, err := windows.LoadLibraryEx(filepath.Clean(path), 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return 0
	}
	addr, err := windows.GetProcAddress(h, "DllGetClassObject")
	if err != nil {
		windows.FreeLibrary(h)
		return 0
	}
	return addr
}

func apoGUID(s string) windows.GUID {
	id, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return id
}

const (
	apoDescriptorSignature = 0x41434453 // ACDS
	apoConnectionSignature = 0x41435053 // ACPS
	apoBufferExternal      = 1
	apoBufferValid         = 1
	apoBufferSilent        = 2
)

type apoInit struct {
	size                        uint32
	class                       windows.GUID
	endpoint, effects, reserved unsafe.Pointer
	devices                     unsafe.Pointer
}

type apoDescriptor struct {
	typ       uint32
	buffer    *float32
	maxFrames uint32
	format    *apoMediaType
	signature uint32
}

type apoConnection struct {
	buffer                   *float32
	frames, flags, signature uint32
}

type windowsDolby struct {
	apo, config, realtime   unsafe.Pointer
	endpoint, fx, devices   unsafe.Pointer
	locked                  bool
	inputChannels           int
	inputMedia, outputMedia apoMediaType
	pin                     runtime.Pinner
	input                   [apoBlockFrames*atmosPCMChannels + 3]float32
	output                  [apoBlockFrames*2 + 3]float32
}

// apoCall keeps Go arguments alive during the COM call.
//
//go:uintptrescapes
func apoCall(obj unsafe.Pointer, slot int, args ...uintptr) uintptr {
	table := *(*unsafe.Pointer)(obj)
	method := *(*uintptr)(unsafe.Add(table, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	var argv [6]uintptr
	argv[0] = uintptr(obj)
	n := copy(argv[1:], args)
	hr, _, _ := syscall.SyscallN(method, argv[:n+1]...)
	return hr
}

func apoOK(hr uintptr) bool { return int32(hr) >= 0 }

func apoRelease(obj unsafe.Pointer) {
	if obj != nil {
		apoCall(obj, 2)
	}
}

func openDolbyEffect(rate, channels int, profile DolbyProfile) (dolbyEffect, int) {
	if rate < 32000 || rate > 192000 || dolbyClassObject() == 0 {
		return nil, 0
	}
	// MyGo serializes reads, seeks and Close on the same decoder goroutine.
	runtime.LockOSThread()
	if hr, _, _ := apoCoInitialize.Call(0, windows.COINIT_MULTITHREADED); !apoOK(hr) {
		runtime.UnlockOSThread()
		return nil, 0
	}
	d := &windowsDolby{inputChannels: channels}
	if !d.open(rate, profile) {
		d.close()
		return nil, 0
	}
	var ticks int64
	if !apoOK(apoCall(d.apo, 4, uintptr(unsafe.Pointer(&ticks)))) || ticks <= 0 || ticks > 1_000_000 {
		d.close()
		return nil, 0
	}
	// APO latency uses 100 ns units and may have been rounded down.
	return d, 2 * int((ticks*int64(rate)+5_000_000)/10_000_000)
}

func (d *windowsDolby) open(rate int, profile DolbyProfile) bool {
	var factory unsafe.Pointer
	hr, _, _ := syscall.SyscallN(dolbyClassObject(), uintptr(unsafe.Pointer(&daxClass)), uintptr(unsafe.Pointer(&classFactoryIID)), uintptr(unsafe.Pointer(&factory)))
	if !apoOK(hr) {
		return false
	}
	defer apoRelease(factory)
	if !apoOK(apoCall(factory, 3, 0, uintptr(unsafe.Pointer(&apoIID)), uintptr(unsafe.Pointer(&d.apo)))) {
		return false
	}
	var enumerator unsafe.Pointer
	hr, _, _ = apoCoCreate.Call(uintptr(unsafe.Pointer(&deviceEnumClass)), 0, windows.CLSCTX_INPROC_SERVER,
		uintptr(unsafe.Pointer(&deviceEnumIID)), uintptr(unsafe.Pointer(&enumerator)))
	if !apoOK(hr) {
		return false
	}
	defer apoRelease(enumerator)
	if !apoOK(apoCall(enumerator, 3, 0, 1, uintptr(unsafe.Pointer(&d.devices)))) || // eRender, DEVICE_STATE_ACTIVE
		!d.openStores(enumerator, rate, profile) {
		return false
	}
	init := apoInit{class: daxClass, endpoint: d.endpoint, effects: d.fx, devices: d.devices}
	init.size = uint32(unsafe.Sizeof(init))
	if !apoOK(apoCall(d.apo, 6, uintptr(init.size), uintptr(unsafe.Pointer(&init)))) ||
		!apoOK(apoCall(d.apo, 0, uintptr(unsafe.Pointer(&apoConfigIID)), uintptr(unsafe.Pointer(&d.config)))) ||
		!apoOK(apoCall(d.apo, 0, uintptr(unsafe.Pointer(&apoRealtimeIID)), uintptr(unsafe.Pointer(&d.realtime)))) {
		return false
	}
	d.inputMedia.init(rate, d.inputChannels)
	d.outputMedia.init(rate, 2)
	d.pin.Pin(&d.inputMedia)
	d.pin.Pin(&d.outputMedia)
	d.pin.Pin(&d.input)
	d.pin.Pin(&d.output)
	input := apoDescriptor{apoBufferExternal, alignedAPOBuffer(&d.input[0]), apoBlockFrames, &d.inputMedia, apoDescriptorSignature}
	output := apoDescriptor{apoBufferExternal, alignedAPOBuffer(&d.output[0]), apoBlockFrames, &d.outputMedia, apoDescriptorSignature}
	in, out := &input, &output
	d.locked = apoOK(apoCall(d.config, 3, 1, uintptr(unsafe.Pointer(&in)), 1, uintptr(unsafe.Pointer(&out))))
	return d.locked
}

type apoPropertyKey struct {
	guid windows.GUID
	id   uint32
}

func (d *windowsDolby) openStores(enumerator unsafe.Pointer, rate int, profile DolbyProfile) bool {
	var device, physical, tuning unsafe.Pointer
	if !apoOK(apoCall(enumerator, 4, 0, 0, uintptr(unsafe.Pointer(&device)))) { // eRender, eConsole
		return false
	}
	defer apoRelease(device)
	if !apoOK(apoCall(device, 4, 0, uintptr(unsafe.Pointer(&physical)))) { // STGM_READ
		return false
	}
	defer apoRelease(physical)
	// The package APO expects a real endpoint GUID during LockForProcess.
	key := apoPropertyKey{endpointProps, 4} // PKEY_AudioEndpoint_GUID
	var value struct {
		typ  uint16
		pad  [3]uint16
		text *uint16
		tail uintptr
	}
	if !apoOK(apoCall(physical, 5, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value)))) {
		return false
	}
	valid := value.typ == 31 && value.text != nil && *value.text != 0 // VT_LPWSTR
	clearAPOProperty.Call(uintptr(unsafe.Pointer(&value)))
	if !valid {
		return false
	}
	for _, out := range []*unsafe.Pointer{&tuning, &d.fx} {
		hr, _, _ := newPropertyStore.Call(uintptr(unsafe.Pointer(&propertyStoreIID)), uintptr(unsafe.Pointer(out)))
		if !apoOK(hr) {
			apoRelease(tuning)
			return false
		}
	}
	defer apoRelease(tuning)
	if !setDolbyProfile(tuning, rate, profile) {
		return false
	}
	stores := [...]unsafe.Pointer{tuning, physical}
	hr, _, _ := mergeAPOStores.Call(uintptr(unsafe.Pointer(&stores[0])), uintptr(len(stores)),
		uintptr(unsafe.Pointer(&propertyStoreIID)), uintptr(unsafe.Pointer(&d.endpoint)))
	return apoOK(hr)
}

func setDolbyProfile(store unsafe.Pointer, rate int, profile DolbyProfile) bool {
	// DAX3 v251 reads these parameters from the instance's property store.
	target := int32(min(max(profile.Target, -34), -18)) * 16 // sixteenths of a dB
	inputTarget := profile.InputTarget
	if inputTarget == 0 {
		inputTarget = -24
	}
	leveler := int32(min(max(profile.Leveler, 0), 10))
	dialogue := int32(min(max(profile.Dialogue, 0), 16))
	eq := int32(min(max(profile.IntelligentEQ, 0), 16))
	ducking := int32(min(max(profile.DialogueDucking, 0), 16))
	if ducking > 0 && dialogue == 0 {
		dialogue = 5 // Generic AAF headphone tuning enables the dialogue detector at this amount.
	}
	bass := int32(min(max(profile.BassBoost, 0), 12) * 16)
	var levelerOn, dialogueOn, eqOn, steerEQ, volumeModelerOn, bassOn, graphicOn int32
	if bass > 0 {
		bassOn = 1
	}
	if profile.graphicActive() {
		graphicOn = 1
	}
	// The bass and graphic EQ stages are skipped in the reduced DAP init mode.
	if (bassOn != 0 || graphicOn != 0) && !setDolbyScalar(store, 60, 0) {
		return false
	}
	outputMode, surround, surroundBoost := int32(1), int32(0), int32(0)
	if profile.Spatial {
		outputMode, surroundBoost = 8, 96 // binaural stereo output
		if !profile.DisableSurroundDecoder {
			surround = 1
		}
	}
	if leveler > 0 {
		levelerOn = 1
	}
	if dialogue > 0 {
		dialogueOn = 1
	}
	if eq > 0 {
		eqOn = 1
		if profile.IntelligentEQSteering {
			steerEQ = 1
		}
	}
	if profile.VolumeModeler {
		volumeModelerOn = 1
	}
	for _, param := range [...]struct {
		id    uint32
		value int32
	}{
		{10, levelerOn}, // volume leveler enable
		{11, leveler},   // volume leveler amount
		{7, int32(min(max(inputTarget, -40), -12) * 16)}, // input target
		{6, target},      // output target
		{39, dialogueOn}, // dialogue enhancer enable
		{38, dialogue},   // dialogue enhancer amount
		{37, ducking},    // dialogue ducking amount
		{9, eqOn},        // intelligent EQ enable
		{12, eq},         // intelligent EQ amount
		{31, steerEQ},    // MI intelligent EQ steering
		{14, bassOn},     // bass enhancer enable
		{15, bass},       // bass boost
		{16, 100},        // bass cutoff in Hz
		{17, 32},         // bass filter width from AAF headphone tuning
		{20, graphicOn},  // graphic EQ enable
		{0, int32(min(max(profile.OutputGain, -24), 6) * 16)}, // DAX system gain for this instance
		{8, volumeModelerOn}, // volume modeler enable
		{5, int32(min(max(profile.VolumeCalibration, -20), 20) * 16)}, // volume modeler calibration
		{40, outputMode}, // output processing mode
		{35, surround},   // surround decoder enable
		{36, surroundBoost},
	} {
		if !setDolbyScalar(store, param.id, param.value) {
			return false
		}
	}
	if eq != 0 && !setDolbyEQBands(store, 13, rate, dolbyIEQTargets(profile.IntelligentEQCurve)) {
		return false
	}
	if graphicOn != 0 {
		var gains [20]int32
		for i, gain := range profile.GraphicGains {
			gains[i] = int32(min(max(gain, -12), 12) * 16)
		}
		return setDolbyEQBands(store, 21, rate, gains)
	}
	return true
}

func setDolbyScalar(store unsafe.Pointer, id uint32, scalar int32) bool {
	key := apoPropertyKey{daxParameters, id}
	value := struct {
		typ      uint16
		reserved [3]uint16
		value    int32
		padding  [3]uint32
	}{typ: 3, value: scalar} // VT_I4
	return apoOK(apoCall(store, 6, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value))))
}

func dolbyIEQTargets(curve string) [20]int32 {
	// The Generic Default DolbyAtmos XML defines the target curves in sixteenths of a dB.
	switch curve {
	case "detailed":
		return [...]int32{150, 142, 188, 216, 189, 195, 202, 199, 210, 225, 230, 236, 235, 235, 214, 165, 112, 49, -24, -217}
	case "warm":
		return [...]int32{114, 146, 183, 169, 170, 128, 103, 90, 98, 126, 127, 140, 96, 85, 80, 66, 38, -32, -132, -275}
	default:
		return [...]int32{157, 167, 218, 218, 203, 188, 192, 192, 205, 213, 218, 209, 193, 159, 134, 97, 71, 22, -90, -283}
	}
}

func setDolbyEQBands(store unsafe.Pointer, id uint32, rate int, targets [20]int32) bool {
	frequencies := DolbyEQFrequencies(rate)
	const n = len(frequencies)
	bands := [3 + 2*n]int32{0: int32(n), 1: int32(n), 2 + n: int32(n)}
	for i, frequency := range frequencies {
		bands[2+i] = min(frequency, int32(rate/2-1))
	}
	copy(bands[3+n:], targets[:])
	key := apoPropertyKey{daxParameters, id}
	value := struct {
		typ      uint16
		reserved [3]uint16
		size     uint32
		data     *int32
	}{typ: 65, size: uint32(unsafe.Sizeof(bands)), data: &bands[0]} // VT_BLOB
	return apoOK(apoCall(store, 6, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&value))))
}

func alignedAPOBuffer(p *float32) *float32 {
	return (*float32)(unsafe.Add(unsafe.Pointer(p), -uintptr(unsafe.Pointer(p))&15))
}

func (d *windowsDolby) process(samples []float32) bool {
	return d.render(samples, samples)
}

func (d *windowsDolby) render(samples, stereo []float32) bool {
	if len(stereo)%2 != 0 || len(stereo) > apoBlockFrames*2 || len(samples) != len(stereo)/2*d.inputChannels {
		return false
	}
	inBuf, outBuf := alignedAPOBuffer(&d.input[0]), alignedAPOBuffer(&d.output[0])
	copy(unsafe.Slice(inBuf, len(samples)), samples)
	frames := uint32(len(stereo) / 2)
	input := apoConnection{inBuf, frames, apoBufferValid, apoConnectionSignature}
	output := apoConnection{buffer: outBuf, signature: apoConnectionSignature}
	in, out := &input, &output
	apoCall(d.realtime, 3, 1, uintptr(unsafe.Pointer(&in)), 1, uintptr(unsafe.Pointer(&out)))
	if output.frames != frames {
		return false
	}
	switch output.flags {
	case apoBufferSilent:
		clear(stereo)
	case apoBufferValid:
		copy(stereo, unsafe.Slice(outBuf, len(stereo)))
	default:
		return false
	}
	return true
}

func (d *windowsDolby) reset() bool { return apoOK(apoCall(d.apo, 3)) }

func (d *windowsDolby) close() {
	if d.locked {
		apoCall(d.config, 4)
	}
	apoRelease(d.realtime)
	apoRelease(d.config)
	apoRelease(d.apo)
	apoRelease(d.fx)
	apoRelease(d.endpoint)
	apoRelease(d.devices)
	d.pin.Unpin()
	windows.CoUninitialize()
	runtime.UnlockOSThread()
}

// Windows exposes IAudioMediaType through a static SDK library. This object
// supplies the corresponding COM interface for stereo float PCM without cgo.
type apoMediaType struct {
	vtable uintptr
	refs   atomic.Uint32
	wave   apoWaveFormat
}

func (m *apoMediaType) init(rate, channels int) {
	mask := uint32(3)
	switch channels {
	case 6:
		mask = 0x3f // FL, FR, FC, LFE, BL, BR
	case atmosPCMChannels:
		mask = 0x563f // 7.1 plus top front left and right
	}
	m.vtable = uintptr(unsafe.Pointer(apoMediaVtable()))
	m.wave = apoWaveFormat{0xfffe, uint16(channels), uint32(rate), uint32(rate * channels * 4), uint16(channels * 4), 32, 22, 32, mask, pcmFloatGUID}
	m.refs.Store(1)
}

type apoWaveFormat struct {
	tag, channels      uint16
	rate, byteRate     uint32
	align, bits, extra uint16
	valid              uint16
	mask               uint32
	subtype            windows.GUID
}

type apoPCMFormat struct {
	subtype               windows.GUID
	channels, bytes, bits uint32
	rate                  float32
	mask                  uint32
}

var apoMediaVtable = sync.OnceValue(func() *[7]uintptr {
	const (
		noInterface = 0x80004002
		nullPointer = 0x80004003
	)
	return &[7]uintptr{
		syscall.NewCallback(func(self *apoMediaType, iid *windows.GUID, out **apoMediaType) uintptr {
			if iid == nil || out == nil {
				return nullPointer
			}
			*out = nil
			if *iid != unknownIID && *iid != mediaTypeIID {
				return noInterface
			}
			self.refs.Add(1)
			*out = self
			return 0
		}),
		syscall.NewCallback(func(self *apoMediaType) uintptr { return uintptr(self.refs.Add(1)) }),
		syscall.NewCallback(func(self *apoMediaType) uintptr { return uintptr(self.refs.Add(^uint32(0))) }),
		syscall.NewCallback(func(_ *apoMediaType, out *uint32) uintptr {
			if out == nil {
				return nullPointer
			}
			*out = 0
			return 0
		}),
		syscall.NewCallback(func(self *apoMediaType, other unsafe.Pointer, flags *uint32) uintptr {
			if other == nil || flags == nil {
				return nullPointer
			}
			var format apoPCMFormat
			*flags = 0
			if apoOK(apoCall(other, 6, uintptr(unsafe.Pointer(&format)))) && format == self.format() {
				*flags = 0x0e // matching format type, format data and user data
				return 0
			}
			return 1 // S_FALSE
		}),
		syscall.NewCallback(func(self *apoMediaType) uintptr { return uintptr(unsafe.Pointer(&self.wave)) }),
		syscall.NewCallback(func(self *apoMediaType, out *apoPCMFormat) uintptr {
			if out == nil {
				return nullPointer
			}
			*out = self.format()
			return 0
		}),
	}
})

func (m *apoMediaType) format() apoPCMFormat {
	return apoPCMFormat{m.wave.subtype, uint32(m.wave.channels), uint32(m.wave.bits) / 8, uint32(m.wave.valid), float32(m.wave.rate), m.wave.mask}
}
