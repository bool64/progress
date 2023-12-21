//go:build !cgo_zstd

package catp

import (
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

func zstdReader(rd io.Reader) (io.Reader, error) {
	if rd, err := zstd.NewReader(rd, zstd.WithDecoderConcurrency(1)); err != nil {
		return nil, fmt.Errorf("failed to init zst reader: %w", err)
	} else {
		return rd, nil
	}
}
