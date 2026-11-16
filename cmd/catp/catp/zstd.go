//go:build !cgo_zstd

package catp

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/klauspost/compress/zstd"
	zstdgo "github.com/vearutop/zstd-go"
)

var expZstd bool // -exp-zstd: use github.com/vearutop/zstd-go.

func zstdReader(rd io.Reader) (io.Reader, error) {
	if expZstd {
		zr := zstdgo.NewReader(rd)
		// ponytail: experiment switch, CATP_ZSTD_CONC=N decodes with an N-block pipeline (0/1: synchronous).
		if n, err := strconv.Atoi(os.Getenv("CATP_ZSTD_CONC")); err == nil {
			zr.Concurrency = n
		}

		return zr, nil
	}

	if rd, err := zstd.NewReader(rd, zstd.WithDecoderConcurrency(1)); err == nil {
		return rd, nil
	} else { //nolint:revive
		return nil, fmt.Errorf("failed to init zst reader: %w", err)
	}
}

func zstdWriter(w io.Writer) (io.WriteCloser, error) {
	if expZstd {
		zw := zstdgo.NewWriter(w, zstdgo.ProfileFastest(), func(w *zstdgo.Writer) {
			w.FrameSize = 16 << 20
			w.Seekable = true
		})

		return zw, nil
	}

	return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithLowerEncoderMem(true))
}
