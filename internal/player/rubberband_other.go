//go:build !windows || !amd64

package player

func loadRubberBand() *rubberBandAPI { return nil }
