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

// Status renders ProgressStatus as a string.
func Status(s progress.ProgressStatus) string {
	if s.Task != "" {
		s.Task += ": "
	}

	res := fmt.Sprintf(s.Task+"%.1f%% bytes read, %.1f MB/s, elapsed %s, remaining %s",
		s.DonePercent, s.SpeedMBPS,
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
		println(Status(status))
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

	pr.Start(st.Size(), func() int64 {
		return cr.Bytes()
	}, filename)
	defer pr.Stop()

	readFile(cr)
}

func main() {
	flag.Parse()
	for i := 0; i < flag.NArg(); i++ {
		cat(flag.Arg(i))
	}
}
