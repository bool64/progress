// Package catp provides catp CLI tool as importable package.
package catp

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bool64/dev/version"
	"github.com/bool64/progress"
	gzip "github.com/klauspost/pgzip"
)

// Main is the entry point for catp CLI tool.
func Main(options ...func(o *Options)) error { //nolint:funlen,cyclop,gocognit,gocyclo,maintidx
	r := &runner{}

	flag.Var(flagFunc(func(v string) error {
		r.filters = append(r.filters, filter{pass: true, ms: v, and: bytes.Split([]byte(v), []byte("^"))})

		return nil
	}), "pass", "filter matching, may contain multiple AND patterns separated by ^,\n"+
		"if filter matches, line is passed to the output (may be filtered out by preceding -skip)\n"+
		"other -pass values are evaluated if preceding pass/skip did not match,\n"+
		"for example, you can use \"-pass bar^baz -pass foo -skip fo\" to only keep lines that have (bar AND baz) OR foo, but not fox")

	flag.BoolFunc("pass-any", "finishes matching and gets the value even if previous -pass did not match,\n"+
		"if previous -skip matched, the line would be skipped any way.", func(s string) error {
		r.filters = append(r.filters, filter{pass: true})

		return nil
	})

	flag.Var(flagFunc(func(v string) error {
		r.filters = append(r.filters, filter{pass: false, ms: v, and: bytes.Split([]byte(v), []byte("^"))})

		return nil
	}), "skip", "filter matching, may contain multiple AND patterns separated by ^,\n"+
		"if filter matches, line is removed from the output (may be kept if it passed preceding -pass)\n"+
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
