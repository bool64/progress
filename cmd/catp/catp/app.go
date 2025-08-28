// Package catp provides catp CLI tool as importable package.
package catp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bool64/dev/version"
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

	// pass is a slice of OR items, that are slices of AND items.
	pass [][][]byte
	// skip is a slice of OR items, that are slices of AND items.
	skip [][][]byte

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

	if len(r.pass) > 0 || len(r.skip) > 0 || r.options.PrepareLine != nil {
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

	for s.Scan() {
		lines++

		if r.limiter != nil {
			_ = r.limiter.Wait(context.Background()) //nolint:errcheck // No failure condition here.
		}

		if lines >= linesPush {
			atomic.AddInt64(&r.currentLines, int64(lines))
			lines = 0
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
	shouldWrite := false

	if len(r.pass) == 0 {
		shouldWrite = true
	} else {
		for _, orFilter := range r.pass {
			orPassed := true

			for _, andFilter := range orFilter {
				if !bytes.Contains(line, andFilter) {
					orPassed = false

					break
				}
			}

			if orPassed {
				shouldWrite = true

				break
			}
		}
	}

	if !shouldWrite {
		return shouldWrite
	}

	for _, orFilter := range r.skip {
		orPassed := true

		for _, andFilter := range orFilter {
			if !bytes.Contains(line, andFilter) {
				orPassed = false

				break
			}
		}

		if orPassed {
			shouldWrite = false

			break
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
		if strings.HasSuffix(fn, ".gz") {
			fn = strings.TrimSuffix(fn, ".gz")
		} else {
			fn = strings.TrimSuffix(fn, ".zst")
		}

		w, err := os.Create(fn) //nolint:gosec
		if err != nil {
			return err
		}

		defer func() {
			if clErr := w.Close(); clErr != nil && err == nil {
				err = clErr
			}
		}()

		out = w
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

	if len(r.pass) > 0 || len(r.skip) > 0 || r.parallel > 1 || r.hasOptions || r.countLines || r.rateLimit > 0 {
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

type stringFlags []string

func (i *stringFlags) String() string {
	return "my string representation"
}

func (i *stringFlags) Set(value string) error {
	*i = append(*i, value)

	return nil
}

// Options allows behavior customisations.
type Options struct {
	// PrepareLine is invoked for every line, if result is nil, line is skipped.
	// You can use buf to avoid allocations for a result, and change its capacity if needed.
	PrepareLine func(filename string, lineNr int, line []byte, buf *[]byte) []byte
}

// Main is the entry point for catp CLI tool.
func Main(options ...func(o *Options)) error { //nolint:funlen,cyclop,gocognit,gocyclo,maintidx
	var (
		pass stringFlags
		skip stringFlags
	)

	r := &runner{}

	flag.Var(&pass, "pass", "filter matching, may contain multiple AND patterns separated by ^,\n"+
		"if filter matches, line is passed to the output (unless filtered out by -skip)\n"+
		"each -pass value is added with OR logic,\n"+
		"for example, you can use \"-pass bar^baz -pass foo\" to only keep lines that have (bar AND baz) OR foo")

	flag.Var(&skip, "skip", "filter matching, may contain multiple AND patterns separated by ^,\n"+
		"if filter matches, line is removed from the output (even if it passed -pass)\n"+
		"each -skip value is added with OR logic,\n"+
		"for example, you can use \"-skip quux^baz -skip fooO\" to skip lines that have (quux AND baz) OR fooO")

	flag.IntVar(&r.parallel, "parallel", 1, "number of parallel readers if multiple files are provided\n"+
		"lines from different files will go to output simultaneously (out of order of files, but in order of lines in each file)\n"+
		"use 0 for multi-threaded zst decoder (slightly faster at cost of more CPU)")

	cpuProfile := flag.String("dbg-cpu-prof", "", "write first 10 seconds of CPU profile to file")
	memProfile := flag.String("dbg-mem-prof", "", "write heap profile to file after 10 seconds")
	output := flag.String("output", "", "output to file (can have .gz or .zst ext for compression) instead of STDOUT")
	flag.BoolVar(&r.noProgress, "no-progress", false, "disable progress printing")
	flag.BoolVar(&r.countLines, "l", false, "count lines")
	flag.Float64Var(&r.rateLimit, "rate-limit", 0, "output rate limit lines per second")
	progressJSON := flag.String("progress-json", "", "write current progress to a file")
	ver := flag.Bool("version", false, "print version and exit")

	flag.StringVar(&r.outDir, "out-dir", "", "output to directory instead of STDOUT\n"+
		"files will be written to out dir with original base names\n"+
		"disables output flag")

	flag.Usage = func() {
		fmt.Println("catp", version.Module("github.com/bool64/progress").Version+",",
			version.Info().GoVersion, strings.Join(versionExtra, " "))
		fmt.Println()
		fmt.Println("catp prints contents of files to STDOUT or dir/file output, \n" +
			"while printing current progress status to STDERR. \n" +
			"It can decompress data from .gz and .zst files.\n" +
			"Use dash (-) as PATH to read STDIN.")
		fmt.Println()
		fmt.Println("Usage of catp:")
		fmt.Println("catp [OPTIONS] PATH ...")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *ver {
		fmt.Println(version.Module("github.com/bool64/progress").Version)

		return nil
	}

	if flag.NArg() == 0 {
		flag.Usage()

		return nil
	}

	if *cpuProfile != "" {
		startProfiling(*cpuProfile, *memProfile)

		defer pprof.StopCPUProfile()
	}

	if len(options) > 0 {
		r.hasOptions = true

		for _, opt := range options {
			opt(&r.options)
		}
	}

	var files []string

	args := flag.Args()
	isStdin := false

	if len(args) == 1 && args[0] == "-" {
		files = append(files, "-") // STDIN
		isStdin = true
	} else {
		for _, f := range args {
			glob, err := filepath.Glob(f)
			if err != nil {
				return err
			}

			for _, f := range glob {
				alreadyThere := false

				for _, e := range files {
					if e == f {
						alreadyThere = true

						break
					}
				}

				if !alreadyThere {
					files = append(files, f)
				}
			}
		}
	}

	sort.Strings(files)

	if *output != "" && r.outDir == "" { //nolint:nestif
		fn := *output

		out, err := os.Create(fn) //nolint:gosec
		if err != nil {
			return fmt.Errorf("failed to create output file %s: %w", fn, err)
		}

		r.output = out
		compCloser := io.Closer(io.NopCloser(nil))

		switch {
		case strings.HasSuffix(fn, ".gz"):
			gw := gzip.NewWriter(r.output)
			compCloser = gw

			r.output = gw
		case strings.HasSuffix(fn, ".zst"):
			zw, err := zstdWriter(r.output)
			if err != nil {
				return fmt.Errorf("zstd new writer: %w", err)
			}

			compCloser = zw

			r.output = zw
		}

		w := bufio.NewWriterSize(r.output, 64*1024)
		r.output = w

		defer func() {
			if err := w.Flush(); err != nil {
				log.Fatalf("failed to flush STDOUT buffer: %s", err)
			}

			if err := compCloser.Close(); err != nil {
				log.Fatalf("failed to close compressor: %s", err)
			}

			if err := out.Close(); err != nil {
				log.Fatalf("failed to close output file %s: %s", *output, err)
			}
		}()
	} else {
		if isStdin {
			r.output = os.Stdout
		} else {
			w := bufio.NewWriterSize(os.Stdout, 64*1024)
			r.output = w

			defer func() {
				if err := w.Flush(); err != nil {
					log.Fatalf("failed to flush STDOUT buffer: %s", err)
				}
			}()
		}
	}

	if len(pass) > 0 {
		for _, orFilter := range pass {
			var og [][]byte
			for _, andFilter := range strings.Split(orFilter, "^") {
				og = append(og, []byte(andFilter))
			}

			r.pass = append(r.pass, og)
		}
	}

	if len(skip) > 0 {
		for _, orFilter := range skip {
			var og [][]byte
			for _, andFilter := range strings.Split(orFilter, "^") {
				og = append(og, []byte(andFilter))
			}

			r.skip = append(r.skip, og)
		}
	}

	r.sizes = make(map[string]int64)
	r.progressJSON = *progressJSON
	r.pr = &progress.Progress{
		Interval:         5 * time.Second,
		IncrementalSpeed: true,
		Print: func(status progress.Status) {
			s := r.st(status)

			if r.noProgress {
				return
			}

			println(s)
		},
	}

	for _, fn := range files {
		if fn == "-" {
			r.totalBytes = -1

			continue
		}

		st, err := os.Stat(fn)
		if err != nil {
			return fmt.Errorf("failed to read file stats %s: %w", fn, err)
		}

		if strings.HasSuffix(fn, ".zst") || strings.HasSuffix(fn, ".gz") {
			r.hasCompression = true
		}

		r.totalBytes += st.Size()
		r.sizes[fn] = st.Size()
	}

	if r.parallel >= 2 {
		pr := r.pr
		pr.Start(func(t *progress.Task) {
			t.TotalBytes = func() int64 { return r.totalBytes }
			t.CurrentBytes = func() int64 { return atomic.LoadInt64(&r.currentBytes) }
			t.CurrentLines = func() int64 { return atomic.LoadInt64(&r.currentLines) }
			t.Task = "all"
			t.PrintOnStart = true
		})

		sem := make(chan struct{}, r.parallel)
		errs := make(chan error, r.parallel)

		for _, fn := range files {
			fn := fn

			select {
			case err := <-errs:
				return err
			case sem <- struct{}{}:
			}

			go func() {
				defer func() {
					<-sem
				}()

				if err := r.cat(fn); err != nil {
					errs <- fmt.Errorf("%s: %w", fn, err)
				}
			}()
		}

		// Wait for goroutines to finish by acquiring all slots.
		for i := 0; i < cap(sem); i++ {
			sem <- struct{}{}
		}

		pr.Stop()

		close(errs)

		var errValues []error

		for err := range errs {
			if err != nil {
				errValues = append(errValues, err)
			}
		}

		if errValues != nil {
			return errors.Join(errValues...)
		}
	} else {
		for _, fn := range files {
			if err := r.cat(fn); err != nil {
				return fmt.Errorf("%s: %w", fn, err)
			}
		}
	}

	return nil
}
