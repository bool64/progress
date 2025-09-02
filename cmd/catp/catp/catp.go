package catp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bool64/progress"
	"github.com/klauspost/compress/zstd"
	gzip "github.com/klauspost/pgzip"
	"golang.org/x/time/rate"
)

var versionExtra []string

type runner struct {
	mu     sync.Mutex
	output io.Writer

	pr           *progress.Progress
	progressJSON string

	sizes      map[string]int64
	matches    int64
	totalBytes int64
	outDir     string

	parallel int

	currentBytes             int64
	currentBytesUncompressed int64
	currentLines             int64

	filters []filter

	currentFile  *progress.CountingReader
	currentTotal int64

	lastErr               error
	lastStatusTime        int64
	lastBytesUncompressed int64

	rateLimit float64
	limiter   *rate.Limiter

	noProgress bool
	countLines bool

	hasOptions bool
	options    Options

	hasCompression bool
}

type (
	filter struct {
		pass bool // Skip is false.
		and  [][]byte
		ms   string
	}
	flagFunc func(v string) error
)

func (f flagFunc) String() string         { return "" }
func (f flagFunc) Set(value string) error { return f(value) }

// humanReadableBytes converts bytes to a human-readable string (TB, GB, MB, KB, or bytes).
func humanReadableBytes(bytes int64) string {
	const (
		Byte  = 1
		KByte = Byte * 1024
		MByte = KByte * 1024
		GByte = MByte * 1024
		TByte = GByte * 1024
	)

	switch {
	case bytes >= TByte:
		return fmt.Sprintf("%.2f TB", float64(bytes)/float64(TByte))
	case bytes >= GByte:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GByte))
	case bytes >= MByte:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MByte))
	case bytes >= KByte:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KByte))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// st renders Status as a string.
func (r *runner) st(s progress.Status) string {
	var res string

	type progressJSON struct {
		progress.Status
		BytesCompleted     int64   `json:"bytes_completed"`
		BytesTotal         int64   `json:"bytes_total"`
		CurrentFilePercent float64 `json:"current_file_percent,omitempty"`
		Matches            *int64  `json:"matches,omitempty"`
		ElapsedSeconds     float64 `json:"elapsed_sec"`
		RemainingSeconds   float64 `json:"remaining_sec"`
	}

	pr := progressJSON{
		Status: s,
	}

	currentBytesUncompressed := atomic.LoadInt64(&r.currentBytesUncompressed)
	currentBytes := atomic.LoadInt64(&r.currentBytes)

	if len(r.sizes) > 1 && r.parallel <= 1 {
		pr.CurrentFilePercent = 100 * float64(r.currentFile.Bytes()) / float64(r.currentTotal)

		if s.LinesCompleted != 0 {
			res = fmt.Sprintf("all: %.1f%% bytes read, %s: %.1f%% bytes read, %d lines processed, %.1f l/s, %.1f MB/s, elapsed %s, remaining %s",
				s.DonePercent, s.Task, pr.CurrentFilePercent, s.LinesCompleted, s.SpeedLPS, s.SpeedMBPS,
				s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String())
		} else {
			res = fmt.Sprintf("all: %.1f%% bytes read, %s: %.1f%% bytes read, %.1f MB/s, elapsed %s, remaining %s",
				s.DonePercent, s.Task, pr.CurrentFilePercent, s.SpeedMBPS,
				s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String())
		}
	} else {
		if s.LinesCompleted != 0 {
			if r.totalBytes == -1 { // STDIN
				res = fmt.Sprintf("%s read, %d lines processed, %.1f l/s, %.1f MB/s, elapsed %s",
					humanReadableBytes(s.BytesCompleted), s.LinesCompleted, s.SpeedLPS, s.SpeedMBPS,
					s.Elapsed.Round(10*time.Millisecond).String())
			} else {
				res = fmt.Sprintf("%s: %.1f%% bytes read, %d lines processed, %.1f l/s, %.1f MB/s, elapsed %s, remaining %s",
					s.Task, s.DonePercent, s.LinesCompleted, s.SpeedLPS, s.SpeedMBPS,
					s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String())
			}
		} else {
			if r.totalBytes == -1 {
				res = fmt.Sprintf("%s read, %.1f MB/s, elapsed %s",
					humanReadableBytes(s.BytesCompleted), s.SpeedMBPS,
					s.Elapsed.Round(10*time.Millisecond).String())
			} else {
				res = fmt.Sprintf("%s: %.1f%% bytes read, %.1f MB/s, elapsed %s, remaining %s",
					s.Task, s.DonePercent, s.SpeedMBPS,
					s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String())
			}
		}
	}

	if currentBytesUncompressed > currentBytes && r.hasCompression {
		lastBytesUncompressed := atomic.LoadInt64(&r.lastBytesUncompressed)
		lastStatusTime := atomic.LoadInt64(&r.lastStatusTime)
		now := time.Now().Unix()

		if lastBytesUncompressed != 0 && lastStatusTime != 0 && lastStatusTime != now {
			spdMPBS := (float64(currentBytesUncompressed-lastBytesUncompressed) / float64(now-lastStatusTime)) / (1024 * 1024)
			res = strings.ReplaceAll(res, "MB/s", fmt.Sprintf("MB/s (uncomp %.1f MB/s)", spdMPBS))
		}

		atomic.StoreInt64(&r.lastStatusTime, now)
		atomic.StoreInt64(&r.lastBytesUncompressed, currentBytesUncompressed)
	}

	if len(r.filters) > 0 || r.options.PrepareLine != nil {
		m := atomic.LoadInt64(&r.matches)
		pr.Matches = &m
		res += fmt.Sprintf(", matches %d", m)
	}

	if r.progressJSON != "" {
		pr.ElapsedSeconds = pr.Elapsed.Truncate(time.Second).Seconds()
		pr.RemainingSeconds = pr.Remaining.Round(time.Second).Seconds()
		pr.BytesCompleted = currentBytes
		pr.BytesTotal = atomic.LoadInt64(&r.totalBytes)

		if j, err := json.Marshal(pr); err == nil {
			if err = os.WriteFile(r.progressJSON, append(j, '\n'), 0o600); err != nil {
				println("failed to write progress JSON: " + err.Error())
			}
		}
	}

	return res
}

func (r *runner) readFile(rd io.Reader, out io.Writer) {
	b := bufio.NewReaderSize(rd, 64*1024)

	_, err := io.Copy(out, b)
	if err != nil {
		log.Fatal(err)
	}
}

func (r *runner) scanFile(filename string, rd io.Reader, out io.Writer) {
	s := bufio.NewScanner(rd)
	s.Buffer(make([]byte, 64*1024), 10*1024*1024)

	lines := 0
	buf := make([]byte, 64*1024)

	linesPush := 1000
	if r.rateLimit < 100 {
		linesPush = 1
	}

	flusher, _ := out.(interface {
		Flush() error
	})

	for s.Scan() {
		lines++

		if r.limiter != nil {
			_ = r.limiter.Wait(context.Background()) //nolint:errcheck // No failure condition here.
		}

		if lines >= linesPush {
			atomic.AddInt64(&r.currentLines, int64(lines))
			lines = 0

			if flusher != nil {
				if r.parallel > 1 && r.outDir == "" {
					r.mu.Lock()
					if err := flusher.Flush(); err != nil {
						r.lastErr = err
					}
					r.mu.Unlock()
				} else {
					if err := flusher.Flush(); err != nil {
						r.lastErr = err
					}
				}
			}
		}

		line := s.Bytes()

		if !r.shouldWrite(line) {
			continue
		}

		if r.hasOptions {
			if r.options.PrepareLine != nil {
				buf = buf[:0]
				line = r.options.PrepareLine(filename, lines, line, &buf)
			}

			if line == nil {
				continue
			}
		}

		atomic.AddInt64(&r.matches, 1)

		if r.parallel > 1 && r.outDir == "" {
			r.mu.Lock()
		}

		if _, err := out.Write(append(line, '\n')); err != nil {
			r.lastErr = err

			if r.parallel > 1 && r.outDir == "" {
				r.mu.Unlock()
			}

			return
		}

		if r.parallel > 1 && r.outDir == "" {
			r.mu.Unlock()
		}
	}

	atomic.AddInt64(&r.currentLines, int64(lines))

	if err := s.Err(); err != nil {
		r.mu.Lock()
		defer r.mu.Unlock()

		r.lastErr = err
	}
}

func (r *runner) shouldWrite(line []byte) bool {
	shouldWrite := true

	for _, f := range r.filters {
		if f.pass {
			shouldWrite = false
		}

		andMatched := true

		for _, andFilter := range f.and {
			if !bytes.Contains(line, andFilter) {
				andMatched = false

				break
			}
		}

		if andMatched {
			return f.pass
		}
	}

	return shouldWrite
}

func (r *runner) cat(filename string) (err error) { //nolint:gocyclo
	var rd io.Reader

	if filename == "-" {
		rd = os.Stdin
	} else {
		file, err := os.Open(filename) //nolint:gosec
		if err != nil {
			return err
		}

		defer func() {
			if clErr := file.Close(); clErr != nil && err == nil {
				err = clErr
			}
		}()

		rd = io.Reader(file)
	}

	if !r.noProgress {
		cr := progress.NewCountingReader(rd)
		cr.SetBytes(&r.currentBytes)
		cr.SetLines(nil)

		if r.parallel <= 1 {
			cr = progress.NewCountingReader(rd)
			cr.SetLines(nil)
			r.currentFile = cr
			r.currentTotal = r.sizes[filename]
		}

		rd = cr
	}

	if rd, err = r.openReader(rd, filename); err != nil {
		return err
	}

	if !r.noProgress {
		crl := progress.NewCountingReader(rd)

		crl.SetBytes(&r.currentBytesUncompressed)
		crl.SetLines(nil)

		rd = crl
	}

	out := r.output

	if r.outDir != "" {
		fn := r.outDir + "/" + path.Base(filename)

		w, err := os.Create(fn) //nolint:gosec
		if err != nil {
			return err
		}
		defer func() {
			println("closing file")

			if clErr := w.Close(); clErr != nil && err == nil {
				err = clErr
			}
		}()

		out = w

		if strings.HasSuffix(fn, ".gz") {
			z := gzip.NewWriter(w)
			out = z

			defer func() {
				if clErr := z.Close(); clErr != nil && err == nil {
					err = clErr
				}
			}()
		} else {
			z, err := zstdWriter(w)
			if err != nil {
				return err
			}
			out = z

			defer func() {
				println("closing zstdWriter")
				if clErr := z.Close(); clErr != nil && err == nil {
					err = clErr
				}
			}()
		}
	}

	if r.parallel <= 1 && !r.noProgress {
		r.pr.Start(func(t *progress.Task) {
			t.TotalBytes = func() int64 {
				return r.totalBytes
			}

			t.CurrentBytes = r.currentFile.Bytes
			t.CurrentLines = func() int64 { return atomic.LoadInt64(&r.currentLines) }
			t.Task = filename
			t.Continue = true
			t.PrintOnStart = true
		})
	}

	if r.rateLimit > 0 {
		r.limiter = rate.NewLimiter(rate.Limit(r.rateLimit), 100)
	}

	if len(r.filters) > 0 || r.parallel > 1 || r.hasOptions || r.countLines || r.rateLimit > 0 {
		r.scanFile(filename, rd, out)
	} else {
		r.readFile(rd, out)
	}

	if r.parallel <= 1 && !r.noProgress {
		r.pr.Stop()
	}

	return r.lastErr
}

func (r *runner) openReader(rd io.Reader, filename string) (io.Reader, error) {
	var err error

	switch {
	case strings.HasSuffix(filename, ".gz"):
		if rd, err = gzip.NewReader(rd); err != nil {
			return nil, fmt.Errorf("failed to init gzip reader: %w", err)
		}
	case strings.HasSuffix(filename, ".zst"):
		if r.parallel >= 1 {
			if rd, err = zstdReader(rd); err != nil {
				return nil, fmt.Errorf("failed to init zst reader: %w", err)
			}
		} else {
			if rd, err = zstd.NewReader(rd); err != nil {
				return nil, fmt.Errorf("failed to init zst reader: %w", err)
			}
		}
	}

	return rd, nil
}

func startProfiling(cpuProfile string, memProfile string) {
	f, err := os.Create(cpuProfile) //nolint:gosec
	if err != nil {
		log.Fatal(err)
	}

	if err = pprof.StartCPUProfile(f); err != nil {
		log.Fatal(err)
	}

	go func() {
		time.Sleep(10 * time.Second)
		pprof.StopCPUProfile()
		println("CPU profile written to", cpuProfile)

		if memProfile != "" {
			f, err := os.Create(memProfile) //nolint:gosec
			if err != nil {
				log.Fatal(err)
			}

			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Fatal("writing heap profile:", err)
			}

			println("Memory profile written to", memProfile)
		}
	}()
}

// Options allows behavior customisations.
type Options struct {
	// PrepareLine is invoked for every line, if result is nil, line is skipped.
	// You can use buf to avoid allocations for a result, and change its capacity if needed.
	PrepareLine func(filename string, lineNr int, line []byte, buf *[]byte) []byte
}
