// Package catp provides catp CLI tool as importable package.
package catp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bool64/dev/version"
	"github.com/bool64/progress"
	"github.com/klauspost/compress/zstd"
	gzip "github.com/klauspost/pgzip"
)

type runner struct {
	mu     sync.Mutex
	output io.Writer

	pr           *progress.Progress
	progressJSON string

	sizes      map[string]int64
	matches    int64
	totalBytes int64

	parallel     int
	currentBytes int64
	currentLines int64

	// grep is a slice of AND items, that are slices of OR items.
	grep [][][]byte

	currentFile  *progress.CountingReader
	currentTotal int64

	lastErr error
}

// st renders Status as a string.
func (r *runner) st(s progress.Status) string {
	var res string

	type progressJSON struct {
		progress.Status
		CurrentFilePercent float64 `json:"current_file_percent,omitempty"`
		Matches            *int64  `json:"matches,omitempty"`
		ElapsedSeconds     float64 `json:"elapsed_seconds"`
		RemainingSeconds   float64 `json:"remaining_seconds"`
	}

	pr := progressJSON{
		Status: s,
	}

	if len(r.sizes) > 1 && r.parallel <= 1 {
		pr.CurrentFilePercent = 100 * float64(r.currentFile.Bytes()) / float64(r.currentTotal)

		res = fmt.Sprintf("all: %.1f%% bytes read, %s: %.1f%% bytes read, %d lines processed, %.1f l/s, %.1f MB/s, elapsed %s, remaining %s",
			s.DonePercent, s.Task, pr.CurrentFilePercent, s.LinesCompleted, s.SpeedLPS, s.SpeedMBPS,
			s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String())
	} else {
		res = fmt.Sprintf("%s: %.1f%% bytes read, %d lines processed, %.1f l/s, %.1f MB/s, elapsed %s, remaining %s",
			s.Task, s.DonePercent, s.LinesCompleted, s.SpeedLPS, s.SpeedMBPS,
			s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String())
	}

	if r.grep != nil {
		m := atomic.LoadInt64(&r.matches)
		pr.Matches = &m
		res += fmt.Sprintf(", matches %d", m)
	}

	if r.progressJSON != "" {
		pr.ElapsedSeconds = pr.Elapsed.Seconds()
		pr.RemainingSeconds = pr.Remaining.Seconds()

		if j, err := json.Marshal(pr); err == nil {
			if err = os.WriteFile(r.progressJSON, append(j, '\n'), 0o600); err != nil {
				println("failed to write progress JSON: " + err.Error())
			}
		}
	}

	return res
}

func (r *runner) readFile(rd io.Reader) {
	b := bufio.NewReaderSize(rd, 64*1024)

	_, err := io.Copy(r.output, b)
	if err != nil {
		log.Fatal(err)
	}
}

func (r *runner) scanFile(rd io.Reader) {
	s := bufio.NewScanner(rd)
	s.Buffer(make([]byte, 64*1024), 10*1024*1024)

	lines := 0

	for s.Scan() {
		lines++

		if lines >= 1000 {
			atomic.AddInt64(&r.currentLines, int64(lines))
			lines = 0
		}

		if !r.shouldWrite(s.Bytes()) {
			continue
		}

		atomic.AddInt64(&r.matches, 1)

		if r.parallel > 1 {
			r.mu.Lock()
		}

		if _, err := r.output.Write(append(s.Bytes(), '\n')); err != nil {
			r.lastErr = err

			if r.parallel > 1 {
				r.mu.Unlock()
			}

			return
		}

		if r.parallel > 1 {
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

	for _, andGrep := range r.grep {
		andPassed := false

		for _, orGrep := range andGrep {
			if bytes.Contains(line, orGrep) {
				andPassed = true

				break
			}
		}

		if !andPassed {
			shouldWrite = false

			break
		}
	}

	return shouldWrite
}

func (r *runner) cat(filename string) (err error) {
	file, err := os.Open(filename) //nolint:gosec
	if err != nil {
		return err
	}

	defer func() {
		if clErr := file.Close(); clErr != nil && err == nil {
			err = clErr
		}
	}()

	cr := progress.NewCountingReader(file)
	cr.SetBytes(&r.currentBytes)
	cr.SetLines(nil)

	if r.parallel <= 1 {
		cr = progress.NewCountingReader(file)
		cr.SetLines(nil)
		r.currentFile = cr
		r.currentTotal = r.sizes[filename]
	}

	rd := io.Reader(cr)

	if rd, err = r.openReader(rd, filename); err != nil {
		return err
	}

	if r.parallel <= 1 {
		r.pr.Start(func(t *progress.Task) {
			t.TotalBytes = func() int64 {
				return r.totalBytes
			}

			t.CurrentBytes = r.currentFile.Bytes
			t.CurrentLines = func() int64 { return atomic.LoadInt64(&r.currentLines) }
			t.Task = filename
			t.Continue = true
		})
	}

	if len(r.grep) > 0 {
		r.scanFile(rd)
	} else {
		r.readFile(rd)
	}

	cr.Close()

	if r.parallel <= 1 {
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

// Main is the entry point for catp CLI tool.
func Main() error { //nolint:funlen,cyclop,gocognit,gocyclo
	var grep stringFlags

	flag.Var(&grep, "grep", "grep pattern, may contain multiple OR patterns separated by \\|,\n"+
		"each -grep value is added with AND logic, akin to extra '| grep foo',\n"+
		"for example, you can use '-grep bar\\|baz -grep foo' to only keep lines that have (bar OR baz) AND foo")

	parallel := flag.Int("parallel", 1, "number of parallel readers if multiple files are provided\n"+
		"use 0 for multi-threaded zst decoder (slightly faster at cost of more CPU)")
	cpuProfile := flag.String("dbg-cpu-prof", "", "write first 10 seconds of CPU profile to file")
	memProfile := flag.String("dbg-mem-prof", "", "write heap profile to file after 10 seconds")
	output := flag.String("output", "", "output to file instead of STDOUT")
	noProgress := flag.Bool("no-progress", false, "disable progress printing")
	progressJSON := flag.String("progress-json", "", "write current progress to a file")
	ver := flag.Bool("version", false, "print version and exit")

	flag.Parse()

	if *ver {
		fmt.Println(version.Module("github.com/bool64/progress").Version)

		return nil
	}

	if *cpuProfile != "" {
		startProfiling(*cpuProfile, *memProfile)

		defer pprof.StopCPUProfile()
	}

	r := &runner{}

	r.parallel = *parallel

	if *output != "" { //nolint:nestif
		out, err := os.Create(*output)
		if err != nil {
			return fmt.Errorf("failed to create output file %s: %w", *output, err)
		}

		w := bufio.NewWriterSize(out, 128*1024)
		r.output = w

		defer func() {
			if err := w.Flush(); err != nil {
				log.Fatalf("failed to flush STDOUT buffer: %s", err)
			}

			if err := out.Close(); err != nil {
				log.Fatalf("failed to close output file %s: %s", *output, err)
			}
		}()
	} else {
		w := bufio.NewWriterSize(os.Stdout, 128*1024)
		r.output = w

		defer func() {
			if err := w.Flush(); err != nil {
				log.Fatalf("failed to flush STDOUT buffer: %s", err)
			}
		}()
	}

	if len(grep) > 0 {
		for _, andGrep := range grep {
			var og [][]byte
			for _, orGrep := range strings.Split(andGrep, "\\|") {
				og = append(og, []byte(orGrep))
			}

			r.grep = append(r.grep, og)
		}
	}

	r.sizes = make(map[string]int64)
	r.progressJSON = *progressJSON
	r.pr = &progress.Progress{
		Interval: 5 * time.Second,
		Print: func(status progress.Status) {
			s := r.st(status)

			if *noProgress {
				return
			}

			println(s)
		},
	}

	for i := 0; i < flag.NArg(); i++ {
		fn := flag.Arg(i)

		st, err := os.Stat(fn)
		if err != nil {
			return fmt.Errorf("failed to read file stats %s: %w", fn, err)
		}

		r.totalBytes += st.Size()
		r.sizes[fn] = st.Size()
	}

	if *parallel >= 2 {
		pr := r.pr
		pr.Start(func(t *progress.Task) {
			t.TotalBytes = func() int64 { return r.totalBytes }
			t.CurrentBytes = func() int64 { return atomic.LoadInt64(&r.currentBytes) }
			t.CurrentLines = func() int64 { return atomic.LoadInt64(&r.currentLines) }
			t.Task = "all"
		})

		sem := make(chan struct{}, *parallel)
		errs := make(chan error, 1)

		for i := 0; i < flag.NArg(); i++ {
			i := i
			select {
			case err := <-errs:
				return err
			case sem <- struct{}{}:
			}

			go func() {
				defer func() {
					<-sem
				}()

				if err := r.cat(flag.Arg(i)); err != nil {
					errs <- err
				}
			}()
		}

		// Wait for goroutines to finish by acquiring all slots.
		for i := 0; i < cap(sem); i++ {
			sem <- struct{}{}
		}

		pr.Stop()
	} else {
		for i := 0; i < flag.NArg(); i++ {
			if err := r.cat(flag.Arg(i)); err != nil {
				return err
			}
		}
	}

	return nil
}
