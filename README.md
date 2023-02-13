# progress

[![Build Status](https://github.com/bool64/progress/workflows/test-unit/badge.svg)](https://github.com/bool64/progress/actions?query=branch%3Amaster+workflow%3Atest-unit)
[![Coverage Status](https://codecov.io/gh/bool64/progress/branch/master/graph/badge.svg)](https://codecov.io/gh/bool64/progress)
[![GoDevDoc](https://img.shields.io/badge/dev-doc-00ADD8?logo=go)](https://pkg.go.dev/github.com/bool64/progress)
[![Time Tracker](https://wakatime.com/badge/github/bool64/progress.svg)](https://wakatime.com/badge/github/bool64/progress)
![Code lines](https://sloc.xyz/github/bool64/progress/?category=code)
![Comments](https://sloc.xyz/github/bool64/progress/?category=comments)

This library instruments `io.Reader`/`io.Writer` with progress printer.

## Usage

See [`catp`](./cmd/catp/main.go) for an example.

```go
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

	pr.Start(st.Size, cr.Bytes, cr.Lines, filename)
	defer pr.Stop()

	readFile(cr)
}
```
