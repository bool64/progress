package main

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
	"time"

	"github.com/bool64/dev/version"
	"github.com/bool64/progress"
	"github.com/klauspost/compress/zstd"
	gzip "github.com/klauspost/pgzip"
)

type runner struct {
	pr         *progress.Progress
	sizes      map[string]int64
	readBytes  int64
	totalBytes int64

	grep    [][]byte
	reverse bool

	currentFile  *progress.CountingReader
	currentTotal int64
}

// st renders ProgressStatus as a string.
func (r *runner) st(s progress.ProgressStatus) string {
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

	return res
}

func (r *runner) readFile(rd io.Reader) {
	b := bufio.NewReaderSize(rd, 64*1024)

	_, err := io.Copy(os.Stdout, b)
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
				_, _ = os.Stdout.Write(s.Bytes())

				break
			}
		}
	}

	if err := s.Err(); err != nil {
		log.Fatal(err)
	}
}

func (r *runner) cat(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	r.currentFile = &progress.CountingReader{Reader: file}
	r.currentTotal = r.sizes[filename]
	rd := io.Reader(r.currentFile)

	switch {
	case strings.HasSuffix(filename, ".gz"):
		if rd, err = gzip.NewReader(rd); err != nil {
			log.Fatalf("failed to init gzip reader: %s", err)
		}
	case strings.HasSuffix(filename, ".zst"):
		if rd, err = zstd.NewReader(rd); err != nil {
			log.Fatalf("failed to init gzip reader: %s", err)
		}
	}

	if r.reverse {
	}

	r.pr.Start(func(t *progress.Task) {
		t.TotalBytes = func() int64 {
			return r.totalBytes
		}
		t.CurrentBytes = func() int64 {
			return r.readBytes + r.currentFile.Bytes()
		}
		t.CurrentLines = r.currentFile.Lines
		t.Task = filename
		t.Continue = true
	})

	if len(r.grep) > 0 || r.reverse {
		r.scanFile(rd)
	} else {
		r.readFile(rd)
	}

	r.pr.Stop()
	r.readBytes += r.currentFile.Bytes()
}

func main() {
	grep := flag.String("grep", "", "grep pattern, may contain multiple patterns separated by \\|")
	cpuProfile := flag.String("dbg-cpu-prof", "", "write first 10 seconds of CPU profile to file")
	ver := flag.Bool("version", false, "print version and exit")
	verbosity := flag.Int("verbosity", 1, "progress status verbosity level (0, 1, 2)")

	flag.Parse()

	if *ver {
		fmt.Println(version.Info().Version)

		return
	}

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile) //nolint:gosec
		if err != nil {
			log.Fatal(err)
		}

		if err = pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}

		go func() {
			time.Sleep(10 * time.Second)
			pprof.StopCPUProfile()
			println("CPU profile written to", *cpuProfile)
		}()
	}

	r := &runner{}

	if *grep != "" {
		for _, s := range strings.Split(*grep, "\\|") {
			r.grep = append(r.grep, []byte(s))
		}
	}

	r.sizes = make(map[string]int64)
	r.pr = &progress.Progress{
		Interval: 5 * time.Second,
		Print: func(status progress.ProgressStatus) {
			if *verbosity == 0 {
				return
			}

			println(r.st(status))
		},
	}

	for i := 0; i < flag.NArg(); i++ {
		fn := flag.Arg(i)

		st, err := os.Stat(fn)
		if err != nil {
			log.Fatalf("failed to read file stats %s: %s", fn, err)
		}

		r.totalBytes += st.Size()
		r.sizes[fn] = st.Size()
	}

	for i := 0; i < flag.NArg(); i++ {
		r.cat(flag.Arg(i))
	}
}
