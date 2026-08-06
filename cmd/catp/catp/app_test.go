package catp_test

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/bool64/progress/cmd/catp/catp"
)

// runCapturingStderr runs fn with os.Stderr redirected, returning whatever it wrote.
func runCapturingStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	orig := os.Stderr
	os.Stderr = w

	fnErr := fn()

	os.Stderr = orig

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	captured, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	return string(captured), fnErr
}

// resetFlags clears the global flag set so catp.Main can be invoked again within the same test binary.
func resetFlags() {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
}

func Test_Main(t *testing.T) {
	os.Args = []string{
		"catp",
		"-skip", "dbg",
		"-pass", "linux^64",
		"-pass", "windows",
		"-output", "testdata/filtered.log",
		"testdata/release-assets.yml",
	}

	if err := catp.Main(); err != nil {
		t.Fatal(err)
	}

	d, err := os.ReadFile("testdata/filtered.log")
	if err != nil {
		t.Fatal(err)
	}

	if string(d) != `          curl -sL https://storage.googleapis.com/go-build-snap/go/linux-amd64/$(git ls-remote https://github.com/golang/go.git HEAD | awk '{print $1;}').tar.gz -o gotip.tar.gz
      - name: Upload linux_amd64.tar.gz
        if: hashFiles('linux_amd64.tar.gz') != ''
          asset_path: ./linux_amd64.tar.gz
          asset_name: linux_amd64.tar.gz
      - name: Upload linux_arm64.tar.gz
        if: hashFiles('linux_arm64.tar.gz') != ''
          asset_path: ./linux_arm64.tar.gz
          asset_name: linux_arm64.tar.gz
      - name: Upload windows_amd64.zip
        if: hashFiles('windows_amd64.zip') != ''
          asset_path: ./windows_amd64.zip
          asset_name: windows_amd64.zip
` {
		t.Fatal("Unexpected output:\n", string(d))
	}
}

func Test_Main_samples_seekable(t *testing.T) {
	src, err := os.ReadFile("testdata/release-assets.yml")
	if err != nil {
		t.Fatal(err)
	}

	srcLines := make(map[string]bool)
	for l := range strings.SplitSeq(strings.TrimRight(string(src), "\n"), "\n") {
		srcLines[l] = true
	}

	out := "testdata/samples-seekable.log"

	resetFlags()

	os.Args = []string{
		"catp",
		"-samples", "5",
		"-no-progress",
		"-output", out,
		"testdata/release-assets.yml",
	}

	if err := catp.Main(); err != nil {
		t.Fatal(err)
	}

	defer os.Remove(out) //nolint:errcheck

	d, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(string(d), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 sample lines, got %d: %q", len(lines), string(d))
	}

	for _, l := range lines {
		if !srcLines[l] {
			t.Fatalf("sampled line not found in source file: %q", l)
		}
	}
}

// Test_Main_samples_compressed uses a synthetic file large enough, and stored without
// compression, that it clears pgzip's default read-ahead window (16 blocks of 250KB, ~4MB)
// several times over: -samples on compressed files uses compressed bytes read as a coarse proxy
// for position, so a file that fits inside that read-ahead window would have it all read before
// the scanner advances past the first few lines, clustering every sample at the start. That
// read-ahead is negligible against the multi-GB files -samples is meant for, so this only needs
// a large fixture, not a precise one - the assertions below stay loose accordingly.
func Test_Main_samples_compressed(t *testing.T) {
	gzPath := "testdata/samples-src.log.gz"

	const totalLines = 2000000

	func() {
		f, err := os.Create(gzPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck

		gw, err := gzip.NewWriterLevel(f, gzip.NoCompression)
		if err != nil {
			t.Fatal(err)
		}
		defer gw.Close() //nolint:errcheck

		bw := bufio.NewWriterSize(gw, 64*1024)

		for i := range totalLines {
			if _, err := fmt.Fprintf(bw, "line %08d\n", i); err != nil {
				t.Fatal(err)
			}
		}

		if err := bw.Flush(); err != nil {
			t.Fatal(err)
		}
	}()

	defer os.Remove(gzPath) //nolint:errcheck

	out := "testdata/samples-compressed.log"

	resetFlags()

	os.Args = []string{
		"catp",
		"-samples", "5",
		"-no-progress",
		"-output", out,
		gzPath,
	}

	if err := catp.Main(); err != nil {
		t.Fatal(err)
	}

	defer os.Remove(out) //nolint:errcheck

	d, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(string(d), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 sample lines, got %d: %q", len(lines), string(d))
	}

	prev := -1

	for _, l := range lines {
		n, err := strconv.Atoi(strings.TrimPrefix(l, "line "))
		if err != nil {
			t.Fatalf("unexpected sample line %q: %s", l, err)
		}

		if n <= prev {
			t.Fatalf("samples are not increasing: line %d came after line %d", n, prev)
		}

		prev = n
	}

	// Loose check that samples spread across the file rather than clustering near the start,
	// which is what pgzip's read-ahead outrunning the scanner looked like before the fixture
	// was sized past its read-ahead window.
	if prev < totalLines/4 {
		t.Fatalf("samples did not spread past the first quarter of the file: last sample was line %d of %d", prev, totalLines)
	}
}

func Test_Main_samples_stdin(t *testing.T) {
	resetFlags()

	os.Args = []string{
		"catp",
		"-samples", "2",
		"-no-progress",
		"-",
	}

	if err := catp.Main(); err == nil {
		t.Fatal("expected error sampling stdin")
	}
}

// Test_Main_samples_filtered_uncompressed checks that -pass/-skip filters on an uncompressed
// file still use the seek-based sampler: it scans forward from each target offset only until a
// line passing filters is found, then jumps to the next target - so only matching lines are
// sampled, without falling back to a full scan.
func Test_Main_samples_filtered_uncompressed(t *testing.T) {
	srcPath := "testdata/samples-filtered-src.log"

	const totalLines = 2000000

	func() {
		f, err := os.Create(srcPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck

		bw := bufio.NewWriterSize(f, 64*1024)

		for i := range totalLines {
			if i%97 == 0 {
				if _, err := fmt.Fprintf(bw, "MATCH line %08d\n", i); err != nil {
					t.Fatal(err)
				}
			} else if _, err := fmt.Fprintf(bw, "noise line %08d\n", i); err != nil {
				t.Fatal(err)
			}
		}

		if err := bw.Flush(); err != nil {
			t.Fatal(err)
		}
	}()

	defer os.Remove(srcPath) //nolint:errcheck

	out := "testdata/samples-filtered-uncompressed.log"

	resetFlags()

	os.Args = []string{
		"catp",
		"-samples", "5",
		"-pass", "MATCH",
		"-no-progress",
		"-output", out,
		srcPath,
	}

	stderr, mainErr := runCapturingStderr(t, func() error { return catp.Main() })
	if mainErr != nil {
		t.Fatal(mainErr)
	}

	if strings.Contains(stderr, "full scan") {
		t.Fatalf("expected the seek-based sampler to run, but got a full-scan warning: %q", stderr)
	}

	defer os.Remove(out) //nolint:errcheck

	d, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(string(d), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 sample lines, got %d: %q", len(lines), string(d))
	}

	prev := -1

	for _, l := range lines {
		if !strings.HasPrefix(l, "MATCH line ") {
			t.Fatalf("sample line did not pass -pass filter: %q", l)
		}

		n, err := strconv.Atoi(strings.TrimPrefix(l, "MATCH line "))
		if err != nil {
			t.Fatalf("unexpected sample line %q: %s", l, err)
		}

		if n <= prev {
			t.Fatalf("samples are not increasing: line %d came after line %d", n, prev)
		}

		prev = n
	}

	if prev < totalLines/4 {
		t.Fatalf("samples did not spread past the first quarter of the file: last sample was line %d of %d", prev, totalLines)
	}
}

// Test_Main_samples_gap checks that a stretch with no passing lines is simply skipped rather
// than causing the next passing line (wherever it lands) to also grab the very next one right
// after it to "catch up" - the bucket a sample is taken from should track how far bytes read
// has actually progressed, not the count of samples taken so far.
func Test_Main_samples_gap(t *testing.T) {
	srcPath := "testdata/samples-gap-src.log"

	const (
		totalLines = 2000000
		matchEvery = 97
		gapStart   = totalLines * 6 / 10
		gapEnd     = totalLines * 8 / 10
	)

	func() {
		f, err := os.Create(srcPath)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close() //nolint:errcheck

		bw := bufio.NewWriterSize(f, 64*1024)

		for i := range totalLines {
			if i%matchEvery == 0 && (i < gapStart || i >= gapEnd) {
				if _, err := fmt.Fprintf(bw, "MATCH line %08d\n", i); err != nil {
					t.Fatal(err)
				}
			} else if _, err := fmt.Fprintf(bw, "noise line %08d\n", i); err != nil {
				t.Fatal(err)
			}
		}

		if err := bw.Flush(); err != nil {
			t.Fatal(err)
		}
	}()

	defer os.Remove(srcPath) //nolint:errcheck

	out := "testdata/samples-gap.log"

	resetFlags()

	os.Args = []string{
		"catp",
		"-samples", "10",
		"-pass", "MATCH",
		"-no-progress",
		"-output", out,
		srcPath,
	}

	stderr, mainErr := runCapturingStderr(t, func() error { return catp.Main() })
	if mainErr != nil {
		t.Fatal(mainErr)
	}

	if strings.Contains(stderr, "full scan") {
		t.Fatalf("expected the seek-based sampler to run, but got a full-scan warning: %q", stderr)
	}

	defer os.Remove(out) //nolint:errcheck

	d, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(string(d), "\n"), "\n")

	// The 20%-wide gap should cost at least one of the 10 requested samples, and none of the
	// samples should land inside the gap.
	if len(lines) < 1 || len(lines) >= 10 {
		t.Fatalf("expected fewer than 10 sample lines due to the gap, got %d: %q", len(lines), string(d))
	}

	prev := -1

	for _, l := range lines {
		n, err := strconv.Atoi(strings.TrimPrefix(l, "MATCH line "))
		if err != nil {
			t.Fatalf("unexpected sample line %q: %s", l, err)
		}

		if n >= gapStart && n < gapEnd {
			t.Fatalf("sample landed inside the gap: line %d", n)
		}

		// The bug this guards against: a gap causing the next passing line to also grab the
		// very next matching line right after it, matchEvery lines away, to "catch up".
		if prev >= 0 && n-prev == matchEvery {
			t.Fatalf("samples grabbed back-to-back matches right after the gap: line %d then %d", prev, n)
		}

		prev = n
	}
}
