package catp

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	gzip "github.com/klauspost/pgzip"
)

func makeWriter(fn string) (io.Writer, func() error, error) {
	f, err := os.Create(fn) //nolint:gosec
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create output file %s: %w", fn, err)
	}

	var res io.Writer
	res = f
	compCloser := io.Closer(io.NopCloser(nil))

	switch {
	case strings.HasSuffix(fn, ".gz"):
		gw := gzip.NewWriter(res)
		compCloser = gw

		res = gw
	case strings.HasSuffix(fn, ".zst"):
		zw, err := zstdWriter(res)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd new writer: %w", err)
		}

		compCloser = zw

		res = zw
	}

	w := bufio.NewWriterSize(res, 64*1024)
	res = w

	return res, func() error {
		if err := w.Flush(); err != nil {
			return fmt.Errorf("failed to flush output buffer %s: %w", fn, err)
		}

		if err := compCloser.Close(); err != nil {
			return fmt.Errorf("failed to close compressor %s: %w", fn, err)
		}

		if err := f.Close(); err != nil {
			return fmt.Errorf("failed to close output file %s: %w", fn, err)
		}

		return nil
	}, nil
}
