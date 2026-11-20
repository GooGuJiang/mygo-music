//go:build !windows || !amd64

package player

import "context"

func decodeCodecPCM(context.Context, string, string, string) (int, error) {
	return 0, errNativeCodec
}
