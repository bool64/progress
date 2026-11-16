//go:build !cgo_zstd

package catp

import (
	"io"

	zstdgo "github.com/vearutop/zstd-go"
)

// zstdReader decodes zstd, -parallel 0 enables a 2-block decoding pipeline (faster, more CPU).
func zstdReader(rd io.Reader, parallel int) (io.Reader, error) {
	zr := zstdgo.NewReader(rd)
	if parallel == 0 {
		zr.Concurrency = 2
	}

	return zr, nil
}

func zstdWriter(w io.Writer) (io.WriteCloser, error) {
	return zstdgo.NewWriter(w, zstdgo.ProfileFastest(), func(w *zstdgo.Writer) {
		w.FrameSize = 16 << 20
		w.Seekable = true
	}), nil
}
