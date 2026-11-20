//go:build !windows || !amd64

package player

func openDolbyEffect(int, int, DolbyProfile) (dolbyEffect, int) { return nil, 0 }
