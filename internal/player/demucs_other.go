//go:build !windows || !amd64

package player

import (
	"context"
	"errors"
)

func findDemucs() string { return "" }

func startDemucs(context.Context, string, DemucsMode, int) (*demucsWorker, error) {
	return nil, errors.New("Demucs playback requires Windows x64")
}
