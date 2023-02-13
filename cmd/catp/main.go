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

// st renders ProgressStatus as a string.
func st(s progress.ProgressStatus) string {
	if s.Task != "" {
		s.Task += ": "
	}

	res := fmt.Sprintf(s.Task+"%.1f%% bytes read, %d lines processed, %.1f l/s, %.1f MB/s, elapsed %s, remaining %s",
		s.DonePercent, s.LinesCompleted, s.SpeedLPS, s.SpeedMBPS,
		s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String())

	return res
}

func readFile(r io.Reader) {
	rd := bufio.NewReader(r)

	_, err := io.Copy(os.Stdout, rd)
	if err != nil {
		log.Fatal(err)
	}
}

var pr = &progress.Progress{
	Interval: 5 * time.Second,
	Print: func(status progress.ProgressStatus) {
		println(st(status))
	},
}

func cat(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	st, err := file.Stat()
	if err != nil {
		log.Fatalf("failed to read file stats %s: %s", filename, err)
	}

	cr := &progress.CountingReader{Reader: file}

	pr.Start(st.Size, cr.Bytes, cr.Lines, filename)
	defer pr.Stop()

	readFile(cr)
}

func main() {
	flag.Parse()

	for i := 0; i < flag.NArg(); i++ {
		cat(flag.Arg(i))
	}
}
