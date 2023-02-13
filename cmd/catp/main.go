package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/bool64/progress"
)

type runner struct {
	pr         *progress.Progress
	sizes      map[string]int64
	readBytes  int64
	totalBytes int64

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

func readFile(r io.Reader) {
	rd := bufio.NewReader(r)

	_, err := io.Copy(os.Stdout, rd)
	if err != nil {
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
	readFile(r.currentFile)

	r.pr.Stop()
	r.readBytes += r.currentFile.Bytes()
}

func main() {
	flag.Parse()

	r := &runner{}
	r.sizes = make(map[string]int64)
	r.pr = &progress.Progress{
		Interval: 5 * time.Second,
		Print: func(status progress.ProgressStatus) {
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
