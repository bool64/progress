//go:build cgo_zstd

package catp

import (
	"io"

	zstd "github.com/vearutop/zstd-go/libzstd"
)

func init() {
	versionExtra = append(versionExtra, "CGO_ZSTD")
}

func zstdReader(rd io.Reader, _ int) (io.Reader, error) {
	return zstd.NewReader(rd), nil
}

func zstdWriter(w io.Writer) (io.WriteCloser, error) {
	return zstd.NewWriter(w, zstd.ProfileFastest(), func(w *zstd.Writer) {
		w.FrameSize = 16 << 20
		w.Seekable = true
	}), nil
}
