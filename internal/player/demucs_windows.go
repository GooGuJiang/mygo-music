//go:build windows && amd64

package player

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func findDemucs() string {
	for _, codecs := range bundledCodecDirs() {
		dir := filepath.Join(filepath.Dir(codecs), "demucs")
		worker := filepath.Join(dir, "mygo-demucs.exe")
		_, workerErr := os.Stat(worker)
		_, modelErr := os.Stat(filepath.Join(dir, "native-v1", "htdemucs-f16.gguf"))
		if workerErr == nil && modelErr == nil {
			return worker
		}
	}
	return ""
}

func startDemucs(ctx context.Context, worker string, mode DemucsMode, rate int) (*demucsWorker, error) {
	runtime := filepath.Join(filepath.Dir(worker), "native-v1")
	if mode == DemucsAuto {
		mode = DemucsCUDA
		if _, err := os.Stat(filepath.Join(runtime, string(mode), "ggml-cuda.dll")); err != nil {
			mode = DemucsCPU
		}
	}
	dir := filepath.Join(runtime, string(mode))
	if mode == DemucsCPU && !windows.IsProcessorFeaturePresent(windows.PF_AVX2_INSTRUCTIONS_AVAILABLE) {
		dir = filepath.Join(runtime, "cpu-portable")
	}
	if _, err := os.Stat(filepath.Join(dir, "audiocpp.dll")); err != nil {
		return nil, fmt.Errorf("Demucs %s runtime: %w", mode, err)
	}
	cmd := exec.CommandContext(ctx, worker, "--device", string(mode), "--rate", strconv.Itoa(rate))
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if fd := os.Stderr.Fd(); fd != 0 && fd != uintptr(syscall.InvalidHandle) {
		cmd.Stderr = os.Stderr
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	var readHandle, writeHandle windows.Handle
	// A larger pipe reduces wakeups when transferring PCM and all four stems.
	if err := windows.CreatePipe(&readHandle, &writeHandle, nil, 1<<20); err != nil {
		input.Close()
		return nil, err
	}
	output := os.NewFile(uintptr(readHandle), "demucs stdout")
	childOutput := os.NewFile(uintptr(writeHandle), "demucs stdout writer")
	cmd.Stdout = childOutput
	cmd.Cancel = func() error {
		err := cmd.Process.Kill()
		input.Close()
		output.Close()
		return err
	}
	err = cmd.Start()
	childOutput.Close()
	if err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	closeWorker := func() {
		cmd.Cancel()
		cmd.Wait()
	}
	abort := func() { cmd.Cancel() }
	reader := bufio.NewReader(output)
	timer := time.AfterFunc(20*time.Second, abort)
	line, err := reader.ReadString('\n')
	timer.Stop()
	if err != nil {
		closeWorker()
		return nil, fmt.Errorf("worker failed to initialize (%s): %w", mode, err)
	}
	line = strings.TrimSpace(line)
	rhythm := mode == DemucsCUDA && line == "READY mix-v3 cuda"
	if !rhythm && line != "READY mix-v2 "+string(mode) {
		closeWorker()
		return nil, fmt.Errorf("incompatible Demucs worker response: %q", strings.TrimSpace(line))
	}
	return &demucsWorker{device: mode, rhythm: rhythm, input: input, output: reader, Close: closeWorker, abort: abort}, nil
}
