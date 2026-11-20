//go:build !windows || !amd64

package player

import (
	"context"
	"errors"
)

func findDemucs() string { return "" }

func ImportDemucsCUDA(string) error { return errors.New("CUDA playback requires Windows x64") }

func startDemucs(context.Context, string, DemucsMode, int) (*demucsWorker, error) {
	return nil, errors.New("Demucs playback requires Windows x64")
}
