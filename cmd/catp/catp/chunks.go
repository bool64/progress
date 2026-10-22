package catp

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// minChunkSize avoids splitting small files into pointless tiny chunks.
const minChunkSize = 1 << 20

type chunk struct {
	path       string
	start, end int64
}

type job struct {
	name  string
	chunk *chunk
}

// jobs returns files as is, or, with -split-chunks, uncompressed files split into virtual
// "file.chunkNN" chunks.
func (r *runner) jobs(files []string) []job {
	var res []job

	for _, fn := range files {
		size := r.sizes[fn]
		n := min(int64(r.parallel), size/minChunkSize)

		if !r.splitChunks || n < 2 || strings.HasSuffix(fn, ".gz") || strings.HasSuffix(fn, ".zst") {
			res = append(res, job{name: fn})

			continue
		}

		width := len(strconv.FormatInt(n-1, 10))

		for i := int64(0); i < n; i++ {
			res = append(res, job{
				name:  fmt.Sprintf("%s.chunk%0*d", fn, width, i),
				chunk: &chunk{path: fn, start: size * i / n, end: size * (i + 1) / n},
			})
		}
	}

	return res
}

// chunkReader yields whole lines that start within [start, end) of a file: a line that
// straddles start belongs to the previous chunk, a line that straddles end belongs to this one.
type chunkReader struct {
	br       *bufio.Reader
	left     int64 // Bytes left before end.
	last     byte
	tailDone bool
}

func newChunkReader(f *os.File, start, end int64) (*chunkReader, error) {
	cr := &chunkReader{last: '\n'} // Nothing read yet: no tail to complete.
	pos := start

	if start > 0 {
		// Seek one byte back to see whether start is exactly at the line beginning.
		if _, err := f.Seek(start-1, io.SeekStart); err != nil {
			return nil, err
		}

		pos--
	}

	cr.br = bufio.NewReaderSize(f, 64*1024)

	if start > 0 {
		for {
			b, err := cr.br.ReadSlice('\n')
			pos += int64(len(b))

			if err == bufio.ErrBufferFull {
				continue
			}

			if err != nil && err != io.EOF {
				return nil, err
			}

			break
		}
	}

	cr.left = max(end-pos, 0)

	return cr, nil
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.left > 0 {
		n, err := c.br.Read(p[:min(int64(len(p)), c.left)])
		c.left -= int64(n)

		if n > 0 {
			c.last = p[n-1]
		}

		if err == io.EOF {
			c.left, c.last = 0, '\n'
		}

		return n, err
	}

	if c.last == '\n' || c.tailDone {
		return 0, io.EOF
	}

	// Complete the line that straddles end.
	n := 0
	for n < len(p) {
		b, err := c.br.ReadByte()
		if err != nil {
			c.tailDone = true

			break
		}

		p[n] = b
		n++

		if b == '\n' {
			c.tailDone = true

			break
		}
	}

	if n == 0 {
		return 0, io.EOF
	}

	return n, nil
}
