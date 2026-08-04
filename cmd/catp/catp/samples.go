package catp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"strings"
)

// trySampleSeekable dispatches filename to the instant, seek-based sampler when it's a plain
// (uncompressed) file with no -pass/-skip filters to honor. It reports handled=true when the
// caller should return err as-is: either the fast path ran, or the file is one we can't sample
// this way (stdin has no known size to compute even offsets from). handled=false means the
// caller should fall back to a full-scan sample instead, with reason explaining why for its
// warning message: the file is compressed, or filters need every line inspected anyway.
func (r *runner) trySampleSeekable(filename string) (handled bool, reason string, err error) {
	if filename == "-" {
		return true, "", errors.New("samples: stdin is not supported, provide seekable file paths")
	}

	if strings.HasSuffix(filename, ".gz") || strings.HasSuffix(filename, ".zst") {
		return false, "the file is compressed", nil
	}

	if r.filters.isSet() {
		return false, "-pass/-skip filters are set", nil
	}

	return true, "", r.sampleSeekable(filename)
}

// sampleSeekable collects samples from an uncompressed file by seeking to evenly spaced byte
// offsets and reading the next full line at each, avoiding a full scan of the file.
func (r *runner) sampleSeekable(filename string) (err error) {
	f, err := os.Open(filename) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() {
		if clErr := f.Close(); clErr != nil && err == nil {
			err = clErr
		}
	}()

	out, closer, err := r.sampleOutput(filename)
	if err != nil {
		return err
	}
	defer func() {
		if clErr := closer(); clErr != nil && err == nil {
			err = clErr
		}
	}()

	size := r.sizes[filename]

	return r.sampleAtOffsets(size, out, func(offset int64) ([]byte, error) {
		return readLineAt(f, size, offset)
	})
}

// sampleAtOffsets writes a line for each of r.samples evenly spaced byte offsets across
// [0, size), fetched via lineAt. This is the shared distribution strategy for any format that
// can jump straight to a position instead of scanning: plain files today (lineAt seeks the raw
// file), and seekable zstd later (lineAt would resolve offset through the seek table, decompress
// the covering frame, and locate the line within it). size and offset are in whatever position
// space lineAt understands, e.g. uncompressed offsets for seekable zstd.
func (r *runner) sampleAtOffsets(size int64, out io.Writer, lineAt func(offset int64) ([]byte, error)) error {
	n := r.samples
	if int64(n) > size {
		n = int(size)
	}

	if n <= 0 {
		return nil
	}

	chunk := size / int64(n)

	for i := 0; i < n; i++ {
		line, err := lineAt(int64(i) * chunk)
		if err != nil {
			continue
		}

		if err := r.writeSample(out, line); err != nil {
			return err
		}
	}

	return nil
}

// readLineAt returns the next full line at or after offset, skipping a leading partial line when
// offset lands mid-line. ra is read only via ReadAt (via an io.SectionReader), never Seek, so any
// io.ReaderAt-backed source works without a shared, mutable read position: a plain *os.File today,
// and a seekable zstd's decompressed view later.
func readLineAt(ra io.ReaderAt, size, offset int64) ([]byte, error) {
	br := bufio.NewReader(io.NewSectionReader(ra, offset, size-offset))

	if offset > 0 {
		if _, err := br.ReadBytes('\n'); err != nil {
			return nil, err
		}
	}

	line, err := br.ReadBytes('\n')
	if len(line) == 0 {
		return nil, err
	}

	return bytes.TrimRight(line, "\n"), nil
}

// sampleOutput returns where sample lines should be written, honoring -out-dir the same way cat does.
func (r *runner) sampleOutput(filename string) (io.Writer, func() error, error) {
	if r.outDir == "" {
		return r.output, func() error { return nil }, nil
	}

	return makeWriter(r.outDir + "/" + path.Base(filename))
}

// writeSample writes a single sample line, synchronizing with other goroutines when output is shared.
func (r *runner) writeSample(out io.Writer, line []byte) error {
	synchronize := r.parallel > 1 && r.outDir == ""

	if synchronize {
		r.mu.Lock()
		defer r.mu.Unlock()
	}

	_, err := out.Write(append(line, '\n'))

	return err
}
