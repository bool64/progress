// Package catp provides catp CLI tool as importable package.
package catp

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bool64/dev/version"
	"github.com/bool64/progress"
	"github.com/klauspost/compress/zstd"
	gzip "github.com/klauspost/pgzip"
)

type runner struct {
	output     io.Writer
	pr         *progress.Progress
	sizes      map[string]int64
	readBytes  int64
	readLines  int64
	matches    int64
	totalBytes int64

	grep [][]byte

	currentFile  *progress.CountingReader
	currentTotal int64
	lastErr      error
}

// st renders Status as a string.
func (r *runner) st(s progress.Status) string {
	var res string

	if len(r.sizes) > 1 {
		fileDonePercent := 100 * float64(r.currentFile.Bytes()) / float64(r.currentTotal)
		res = fmt.Sprintf("all: %.1f%% bytes read, %s: %.1f%% bytes read, %d lines processed, %.1f l/s, %.1f MB/s, elapsed %s, remaining %s",
			s.DonePercent, s.Task, fileDonePercent, s.LinesCompleted, s.SpeedLPS, s.SpeedMBPS,
			s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String())
	} else {
		res = fmt.Sprintf("%s: %.1f%% bytes read, %d lines processed, %.1f l/s, %.1f MB/s, elapsed %s, remaining %s",
			s.Task, s.DonePercent, s.LinesCompleted, s.SpeedLPS, s.SpeedMBPS,
			s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String())
	}

	if r.grep != nil {
		res += fmt.Sprintf(", matches %d", atomic.LoadInt64(&r.matches))
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

	for s.Scan() {
		for _, g := range r.grep {
			if bytes.Contains(s.Bytes(), g) {
				if _, err := r.output.Write(append(s.Bytes(), '\n')); err != nil {
					r.lastErr = err

					return
				}

				atomic.AddInt64(&r.matches, 1)

				break
			}
		}
	}

	if err := s.Err(); err != nil {
		r.lastErr = err
	}
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

	r.currentFile = &progress.CountingReader{Reader: file}
	r.currentTotal = r.sizes[filename]
	rd := io.Reader(r.currentFile)
	lines := r.currentFile

	switch {
	case strings.HasSuffix(filename, ".gz"):
		if rd, err = gzip.NewReader(rd); err != nil {
			return fmt.Errorf("failed to init gzip reader: %w", err)
		}
		lines = &progress.CountingReader{Reader: rd}
		rd = lines
	case strings.HasSuffix(filename, ".zst"):
		if rd, err = zstd.NewReader(rd); err != nil {
			return fmt.Errorf("failed to init zst reader: %w", err)
		}
		lines = &progress.CountingReader{Reader: rd}
		rd = lines
	}

	r.pr.Start(func(t *progress.Task) {
		t.TotalBytes = func() int64 {
			return r.totalBytes
		}
		t.CurrentBytes = r.currentFile.Bytes
		t.CurrentLines = lines.Lines

		t.Task = filename
		t.Continue = true
	})

	if len(r.grep) > 0 {
		r.scanFile(rd)
	} else {
		r.readFile(rd)
	}

	r.pr.Stop()

	return r.lastErr
}

func startProfiling(cpuProfile string) {
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
	}()
}

// Main is the entry point for catp CLI tool.
func Main() error { //nolint:funlen,cyclop
	grep := flag.String("grep", "", "grep pattern, may contain multiple patterns separated by \\|")
	cpuProfile := flag.String("dbg-cpu-prof", "", "write first 10 seconds of CPU profile to file")
	output := flag.String("output", "", "output to file instead of STDOUT")
	ver := flag.Bool("version", false, "print version and exit")

	flag.Parse()

	if *ver {
		fmt.Println(version.Module("github.com/bool64/progress").Version)

		return nil
	}

	if *cpuProfile != "" {
		startProfiling(*cpuProfile)
		defer pprof.StopCPUProfile()
	}

	r := &runner{}

	r.output = os.Stdout

	if *output != "" {
		out, err := os.Create(*output)
		if err != nil {
			return fmt.Errorf("failed to create output file %s: %w", *output, err)
		}

		r.output = out

		defer func() {
			if err := out.Close(); err != nil {
				log.Fatalf("failed to close output file %s: %s", *output, err)
			}
		}()
	}

	if *grep != "" {
		for _, s := range strings.Split(*grep, "\\|") {
			r.grep = append(r.grep, []byte(s))
		}
	}

	r.sizes = make(map[string]int64)
	r.pr = &progress.Progress{
		Interval: 5 * time.Second,
		Print: func(status progress.Status) {
			println(r.st(status))
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

	for i := 0; i < flag.NArg(); i++ {
		if err := r.cat(flag.Arg(i)); err != nil {
			return err
		}
	}

	return nil
}
