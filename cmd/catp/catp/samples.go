package catp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"sync/atomic"

	"github.com/bool64/progress"
)

// trySampleSeekable dispatches filename to the seek-based sampler when it's a plain
// (uncompressed) file, filters or no filters. It reports handled=true when the caller should
// return err as-is: either the seek-based path ran, or the file is one we can't sample this way
// (stdin has no known size to compute even offsets from). handled=false means the caller should
// fall back to a full-scan sample instead, because the file is compressed and can't be seeked
// into cheaply.
func (r *runner) trySampleSeekable(filename string) (handled bool, reason string, err error) {
	if filename == "-" {
		return true, "", errors.New("samples: stdin is not supported, provide seekable file paths")
	}

	if strings.HasSuffix(filename, ".gz") || strings.HasSuffix(filename, ".zst") {
		return false, "the file is compressed", nil
	}

	return true, "", r.sampleSeekable(filename)
}

// sampleSeekable collects r.samples lines from an uncompressed file by seeking to evenly spaced
// target offsets and scanning forward from each one for the first line that passes -pass/-skip
// filters (with no filters set, that's simply the next line). Once a line is taken, it jumps
// straight to the next target instead of continuing to read through the rest of the current
// segment - only the (typically short) stretch between a target and its first match is ever
// read, so this stays close to instant even with filters, unlike a full scan. A stretch with no
// passing line before EOF costs that sample rather than blocking for one - fewer samples, not a
// stall - and since all remaining targets are past it too, none of them can find anything either.
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

	size := r.sizes[filename]

	n := r.samples
	if int64(n) > size {
		n = int(size)
	}

	if n <= 0 {
		return nil
	}

	out, closer, err := r.sampleOutput(filename)
	if err != nil {
		return err
	}

	defer func() {
		if clErr := closer(); clErr != nil && err == nil {
			err = clErr
		}
	}()

	// fileBytes tracks our actual position in the file, kept in sync across jumps (set to the
	// seek target immediately, then advanced as each segment is read) so progress display
	// reflects real position rather than lagging behind at the last linearly-read byte.
	var fileBytes int64

	if r.parallel <= 1 && !r.noProgress {
		cr := progress.NewCountingReader(nil)
		cr.SetBytes(&fileBytes)
		r.currentFile = cr
		r.currentTotal = size

		r.pr.Start(func(t *progress.Task) {
			t.TotalBytes = func() int64 { return r.totalBytes }
			t.CurrentBytes = func() int64 { return atomic.LoadInt64(&fileBytes) }
			t.Task = filename
			t.Continue = true
			t.PrintOnStart = true
		})

		defer r.pr.Stop()
	}

	taken := 0

	for taken < n {
		offset := int64(taken) * size / int64(n)
		atomic.StoreInt64(&fileBytes, offset)

		matched, pos, err := r.sampleNextMatch(filename, f, out, offset, size, taken, &fileBytes)
		if err != nil {
			return err
		}

		if !matched {
			break // nothing more from here to EOF; every remaining target is past it too
		}

		taken = min(int(pos*int64(n)/size), n-1) + 1
	}

	return nil
}

// sampleNextMatch scans forward from offset for the first line passing filters, writing it
// (honoring -save-matches and -out-dir/-parallel write synchronization the same way scanFile
// does) and reporting the byte position right after it. matched is false if EOF was reached
// with no passing line.
func (r *runner) sampleNextMatch(
	filename string, ra io.ReaderAt, out io.Writer, offset, size int64, lineNr int, fileBytes *int64,
) (matched bool, pos int64, err error) {
	br := bufio.NewReader(io.NewSectionReader(ra, offset, size-offset))
	pos = offset

	if offset > 0 {
		skip, err := br.ReadBytes('\n')
		pos += int64(len(skip))
		atomic.StoreInt64(fileBytes, pos)

		if err != nil {
			if !errors.Is(err, io.EOF) {
				return false, pos, err
			}

			return false, pos, nil // no full line left from here to EOF
		}
	}

	for {
		raw, rerr := br.ReadBytes('\n')
		pos += int64(len(raw))
		atomic.StoreInt64(fileBytes, pos)

		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return false, pos, rerr
		}

		if len(raw) > 0 {
			line := bytes.TrimRight(raw, "\n")

			save, shouldWrite := r.filters.shouldWrite(line)
			if shouldWrite {
				if r.hasOptions && r.options.PrepareLine != nil {
					var buf []byte

					line = r.options.PrepareLine(filename, lineNr, line, &buf)
				}

				if line != nil {
					w := out
					if save != nil {
						w = save
					}

					if err := r.writeMatch(w, line, save != nil); err != nil {
						return false, pos, err
					}

					atomic.AddInt64(&r.matches, 1)
				}

				return true, pos, nil
			}
		}

		if rerr != nil {
			return false, pos, nil //nolint:nilerr // only io.EOF reaches here, non-EOF already returned above
		}
	}
}

// writeMatch writes a single sample line, synchronizing with other goroutines when output is
// shared: either STDOUT/a shared file (no -out-dir), or a shared -save-matches writer.
func (r *runner) writeMatch(out io.Writer, line []byte, saved bool) error {
	synchronize := r.parallel > 1 && (r.outDir == "" || saved)

	if synchronize {
		r.mu.Lock()
		defer r.mu.Unlock()
	}

	_, err := out.Write(append(line, '\n'))

	return err
}

// sampleOutput returns where sample lines should be written, honoring -out-dir the same way cat does.
func (r *runner) sampleOutput(filename string) (io.Writer, func() error, error) {
	if r.outDir == "" {
		return r.output, func() error { return nil }, nil
	}

	return makeWriter(r.outDir + "/" + path.Base(filename))
}
