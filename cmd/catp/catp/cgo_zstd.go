//go:build cgo_zstd

package catp

import (
	"io"

	"github.com/DataDog/zstd"
)

func init() {
	versionExtra = append(versionExtra, "CGO_ZSTD")
}

func zstdReader(rd io.Reader) (io.Reader, error) {
	return zstd.NewReader(rd), nil
}

func zstdWriter(w io.Writer) (io.WriteCloser, error) {
	return zstd.NewWriterLevel(w, zstd.BestSpeed), nil
}
