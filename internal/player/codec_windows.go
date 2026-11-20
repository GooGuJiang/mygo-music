//go:build windows && amd64

package player

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/ebitengine/purego"
	"github.com/egoist/mygo"
	"golang.org/x/sys/windows"
)

func bundledCodecDirs() []string {
	var dirs []string
	if resources, err := mygo.App.Path(mygo.PathResources); err == nil {
		dirs = append(dirs, filepath.Join(resources, "native-codecs"))
	}
	if mygo.IsDev() {
		// go run and package tests build executables outside the checkout.
		if _, file, _, ok := runtime.Caller(0); ok && filepath.IsAbs(file) {
			dirs = append(dirs, filepath.Join(filepath.Dir(file), "..", "..", "resources", "windows-amd64", "native-codecs"))
		}
	}
	return dirs
}

type nativeCodec struct {
	decode func(input, demuxer string, output *uint16, skip, limit int64, channels *int32, cancel *atomic.Int32) int32
	object func(engine windows.Handle, config, layout, bridge, input, demuxer string, output *uint16, cancel *atomic.Int32) int32
	engine windows.Handle
	dir    string
}

var loadNativeCodec = sync.OnceValues(func() (*nativeCodec, error) {
	dirs := bundledCodecDirs()
	if cache, err := os.UserCacheDir(); err == nil {
		root := filepath.Join(cache, "MyGo Music", "native-codecs")
		dirs = append(dirs, filepath.Join(root, "ffmpeg-8.1.3-39-r3"), filepath.Join(root, "ffmpeg-8.1.3-39-r2"))
	}
	const flags = windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | windows.LOAD_LIBRARY_SEARCH_SYSTEM32
	var dir string
	var module windows.Handle
	var err error = errNativeCodec
	for _, candidate := range dirs {
		if module, err = windows.LoadLibraryEx(filepath.Join(candidate, "mygo-codec.dll"), 0, flags); err == nil {
			dir = candidate
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNativeCodec, err)
	}
	codec := &nativeCodec{dir: dir}
	addr, err := windows.GetProcAddress(module, "decode_pcm_v3")
	if err != nil {
		windows.FreeLibrary(module)
		return nil, fmt.Errorf("%w: %v", errNativeCodec, err)
	}
	purego.RegisterFunc(&codec.decode, addr)
	if addr, err := windows.GetProcAddress(module, "render_atmos_v1"); err == nil {
		if engine, err := windows.LoadLibraryEx(filepath.Join(dir, "orender.dll"), 0, flags); err == nil {
			codec.engine = engine
			purego.RegisterFunc(&codec.object, addr)
		}
	}
	return codec, nil
})

func decodeCodecPCM(ctx context.Context, input, format, output string) (int, error) {
	var channels int32
	err := runCodec(ctx, func(c *nativeCodec, cancel *atomic.Int32) int32 {
		out := windows.StringToUTF16Ptr(output)
		if c.object != nil && (format == "truehd" || format == "eac3" || format == "mov") {
			code := c.object(c.engine, filepath.Join(c.dir, "atmos-render.yaml"), filepath.Join(c.dir, "atmos-7.1.2.yaml"),
				filepath.Join(c.dir, "harletty_dolby_bridge.dll"), input, format, out, cancel)
			if code == 1 {
				channels = atmosPCMChannels
				return 0
			}
			if cancel.Load() != 0 {
				return code
			}
		}
		return c.decode(input, format, out, 0, -1, &channels, cancel)
	})
	return int(channels), err
}

func runCodec(ctx context.Context, run func(*nativeCodec, *atomic.Int32) int32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	codec, err := loadNativeCodec()
	if err != nil {
		return err
	}
	var flag atomic.Int32
	stop := context.AfterFunc(ctx, func() { flag.Store(1) })
	defer stop()
	code := run(codec, &flag)
	if err := ctx.Err(); err != nil {
		return err
	}
	if code < 0 {
		return fmt.Errorf("%w (%d)", errCodecFailed, code)
	}
	return nil
}
