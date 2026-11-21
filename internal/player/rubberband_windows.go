//go:build windows && amd64

package player

import (
	"path/filepath"
	"sync"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/windows"
)

var loadRubberBand = sync.OnceValue(func() *rubberBandAPI {
	const flags = windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | windows.LOAD_LIBRARY_SEARCH_SYSTEM32
	for _, codecs := range bundledCodecDirs() {
		dll := filepath.Join(filepath.Dir(codecs), "rubberband", "mygo-rubberband.dll")
		module, err := windows.LoadLibraryEx(dll, 0, flags)
		if err != nil {
			continue
		}
		api := &rubberBandAPI{}
		bindings := []struct {
			name string
			fn   any
		}{{"open", &api.open}, {"close", &api.close}, {"push", &api.push}, {"pull", &api.pull}}
		for _, binding := range bindings {
			addr, err := windows.GetProcAddress(module, "mygo_stretch_"+binding.name)
			if err != nil {
				break
			}
			purego.RegisterFunc(binding.fn, addr)
		}
		if api.pull != nil {
			return api
		}
		windows.FreeLibrary(module)
	}
	return nil
})
