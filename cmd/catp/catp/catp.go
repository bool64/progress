package catp

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bool64/progress"
	"github.com/klauspost/compress/zstd"
	gzip "github.com/klauspost/pgzip"
	"github.com/vearutop/plot-go"
	"golang.org/x/time/rate"
)

var versionExtra []string

type runner struct {
	mu     sync.Mutex
	output io.Writer

	closed int64

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

	// consumed counts uncompressed bytes of the current file passed through scanFile (nil if not tracked),
	// it is more precise than bytes read by a buffered reader.
	consumed *int64

	filters filters

	currentFile  *progress.CountingReader
	currentTotal int64

	lastErr               error
	lastStatusTime        int64
	lastBytesUncompressed int64

	rateLimit float64
	limiter   *rate.Limiter
	httpAddr  string
	httpAuth  string
	ctx       context.Context

	skipPos      int64
	lastSkipPos  int64
	lastSkipTime int64

	statusMu   sync.Mutex
	lastStatus string
	speed      []speedSample // Kept only with -http, for the status page charts.

	noProgress      bool
	progressInPlace bool
	countLines      bool

	startLine int
	endLine   int
	samples   int

	hasOptions bool
	options    Options

	hasCompression bool

	splitChunks bool
}

type flagFunc func(v string) error

func (f flagFunc) String() string         { return "" }
func (f flagFunc) Set(value string) error { return f(value) }

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

	if r.parallel <= 1 && r.startLine > 0 {
		target := int64(r.startLine)

		if skipPos := atomic.LoadInt64(&r.skipPos); skipPos < target {
			skipInfo := fmt.Sprintf(", skipped %.1f%%", 100*float64(skipPos)/float64(target))

			lastSkipPos := atomic.LoadInt64(&r.lastSkipPos)
			lastSkipTime := atomic.LoadInt64(&r.lastSkipTime)
			now := time.Now().Unix()

			if lastSkipTime != 0 && now > lastSkipTime && skipPos > lastSkipPos {
				lps := float64(skipPos-lastSkipPos) / float64(now-lastSkipTime)
				remain := time.Duration(float64(target-skipPos) / lps * float64(time.Second))
				skipInfo += " remaining " + remain.Round(time.Second).String()
			}

			atomic.StoreInt64(&r.lastSkipPos, skipPos)
			atomic.StoreInt64(&r.lastSkipTime, now)

			res += skipInfo
		}
	}

	if r.filters.isSet() || r.options.PrepareLine != nil {
		m := atomic.LoadInt64(&r.matches)
		pr.Matches = &m
		res += fmt.Sprintf(", matches %d", m)
	}

	if r.progressJSON != "" {
		pr.ElapsedSeconds = pr.Elapsed.Truncate(time.Second).Seconds()
		pr.RemainingSeconds = pr.Remaining.Round(time.Second).Seconds()
		pr.BytesCompleted = s.BytesCompleted // The shared r.currentBytes is not fed with a single reader.
		pr.BytesTotal = atomic.LoadInt64(&r.totalBytes)

		if j, err := json.Marshal(pr); err == nil {
			if err = os.WriteFile(r.progressJSON, append(j, '\n'), 0o600); err != nil {
				println("failed to write progress JSON: " + err.Error())
			}
		}
	}

	r.statusMu.Lock()
	r.lastStatus = res

	if r.httpAddr != "" {
		r.addSample(speedSample{ts: time.Now().Unix(), lps: s.SpeedLPS, mbps: s.SpeedMBPS})
	}

	r.statusMu.Unlock()

	return res
}

func (r *runner) readFile(rd io.Reader, out io.Writer) {
	b := bufio.NewReaderSize(rd, 64*1024)

	_, err := io.Copy(out, b)
	if err != nil {
		log.Fatal(err)
	}
}

// smallInputSize is the total input size below which progress is updated for every line.
const smallInputSize = 10 << 20

// linesPush picks how many lines to batch before updating shared progress counters,
// keeping a batch's fill time under the progress status interval so speed/remaining
// estimates don't alias against a rate-limited throughput.
func (r *runner) linesPush() int {
	// Small inputs are cheap to update per line and would otherwise show no progress for a long time.
	// Compressed input may expand a lot and STDIN has unknown size (-1), those keep batching.
	if total := atomic.LoadInt64(&r.totalBytes); total >= 0 && total < smallInputSize && !r.hasCompression {
		return 1
	}

	if r.limiter == nil {
		return 1000
	}

	lim := r.limiter.Limit()
	if lim == rate.Inf || lim > 1000 {
		return 1000
	}

	if lim < 1 {
		return 1
	}

	return int(lim)
}

// scanFile scans and filters lines from rd to out. When sampleBytes is non-nil, it additionally
// gates writes to r.samples lines evenly spaced across *sampleBytes's range: a line is only
// written once it already passes filters and bytes read so far cross sampleTarget, a plain
// comparison checked on every candidate line. Only once a line actually clears that target is
// the current bucket computed (via one division), so a stretch with no passing lines is simply
// skipped - the line that finally clears a stale target jumps sampleTaken straight to the bucket
// it landed in, instead of also grabbing the very next line to catch up on the buckets that had
// no match.
func (r *runner) scanFile(filename string, rd io.Reader, out io.Writer, sampleBytes *int64) {
	s := bufio.NewScanner(rd)
	s.Buffer(make([]byte, 64*1024), 10*1024*1024)

	atomic.StoreInt64(&r.skipPos, 0)
	atomic.StoreInt64(&r.lastSkipPos, 0)
	atomic.StoreInt64(&r.lastSkipTime, 0)

	fileLines := 0
	lines := 0
	buf := make([]byte, 64*1024)

	linesPush := r.linesPush()

	sampleTotal := 0
	sampleTaken := 0
	sampleSize := int64(0)

	var sampleTarget int64

	if sampleBytes != nil {
		sampleTotal = r.samples
		sampleSize = r.sizes[filename]
	}

	var consumedLocal int64

	for s.Scan() {
		fileLines++
		consumedLocal += int64(len(s.Bytes()) + 1)

		if atomic.LoadInt64(&r.closed) > 0 {
			break
		}

		if r.startLine > 0 && fileLines <= r.startLine {
			if fileLines&0xFFFF == 0 || fileLines == r.startLine {
				atomic.StoreInt64(&r.skipPos, int64(fileLines))
			}

			continue
		}

		if r.endLine > 0 && fileLines > r.endLine {
			break
		}

		lines++

		if r.limiter != nil {
			_ = r.limiter.Wait(r.ctx) //nolint:errcheck // No failure condition here.
		}

		line := s.Bytes()
		w := out

		save, shouldWrite := r.filters.shouldWrite(line)
		if save != nil {
			w = save
		}

		if shouldWrite && sampleBytes != nil {
			switch {
			case sampleTaken >= sampleTotal:
				shouldWrite = false
			case atomic.LoadInt64(sampleBytes) < sampleTarget:
				shouldWrite = false
			default:
				bucket := sampleTotal - 1
				if sampleSize > 0 {
					bucket = min(int(atomic.LoadInt64(sampleBytes)*int64(sampleTotal)/sampleSize), sampleTotal-1)
				}

				sampleTaken = bucket + 1
				sampleTarget = int64(sampleTaken) * sampleSize / int64(sampleTotal)
			}
		}

		if lines >= linesPush {
			atomic.AddInt64(&r.currentLines, int64(lines))
			lines = 0

			if r.consumed != nil {
				atomic.AddInt64(r.consumed, consumedLocal)
				consumedLocal = 0
			}

			linesPush = r.linesPush()

			if flusher, ok := w.(interface {
				Flush() error
			}); ok {
				if r.parallel > 1 && r.outDir == "" {
					r.mu.Lock()
					if err := flusher.Flush(); err != nil {
						r.lastErr = err
					}
					r.mu.Unlock()
				} else {
					if err := flusher.Flush(); err != nil {
						r.lastErr = err
					}
				}
			}
		}

		if !shouldWrite {
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

		synchronize := r.parallel > 1 && (r.outDir == "" || save != nil)

		if synchronize {
			r.mu.Lock()
		}

		if _, err := w.Write(append(line, '\n')); err != nil {
			r.lastErr = err

			if synchronize {
				r.mu.Unlock()
			}

			return
		}

		if synchronize {
			r.mu.Unlock()
		}
	}

	atomic.AddInt64(&r.currentLines, int64(lines))

	if r.consumed != nil {
		atomic.AddInt64(r.consumed, consumedLocal)
	}

	if err := s.Err(); err != nil {
		r.mu.Lock()
		defer r.mu.Unlock()

		r.lastErr = err
	}
}

const maxSamples = 7200

type speedSample struct {
	ts   int64
	lps  float64
	mbps float64
}

// addSample must be called with statusMu held, one sample per second is kept.
func (r *runner) addSample(s speedSample) {
	if n := len(r.speed); n > 0 && r.speed[n-1].ts >= s.ts {
		return
	}

	if len(r.speed) >= maxSamples {
		r.speed = append(r.speed[:0], r.speed[len(r.speed)/2:]...)
	}

	r.speed = append(r.speed, s)
}

// speedJSON returns samples newer than ?since= as [ts, lines/s, MB/s] rows.
func (r *runner) speedJSON(w http.ResponseWriter, req *http.Request) {
	since, _ := strconv.ParseInt(req.FormValue("since"), 10, 64) //nolint:gosec
	rows := [][]any{}

	r.statusMu.Lock()
	for _, s := range r.speed {
		if s.ts > since {
			rows = append(rows, []any{s.ts, s.lps, s.mbps})
		}
	}
	r.statusMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows) //nolint:errchkjson
}

var donePercent = regexp.MustCompile(`(\d+(?:\.\d+)?)% bytes read`)

// pageTitle shows completion in the tab title.
func pageTitle(status string) string {
	if m := donePercent.FindStringSubmatch(status); m != nil {
		return m[1] + "% catp"
	}

	return "catp"
}

func formatLimit(lim rate.Limit) string {
	if lim == rate.Inf {
		return "unlimited"
	}

	return strconv.FormatFloat(float64(lim), 'f', -1, 64)
}

// startControlServer exposes GET/POST /rate-limit to inspect and change the rate limit at runtime.
func (r *runner) startControlServer() error {
	ln, err := net.Listen("tcp", r.httpAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", r.httpAddr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/rate-limit", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			v, err := strconv.ParseFloat(req.FormValue("value"), 64) //nolint:gosec
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}

			if v <= 0 {
				r.limiter.SetLimit(rate.Inf)
			} else {
				r.limiter.SetLimit(rate.Limit(v))
			}
		}

		_, _ = fmt.Fprintf(w, "%s\n", formatLimit(r.limiter.Limit()))
	})

	mux.Handle("/plot/", http.StripPrefix("/plot/", plot.Handler()))
	mux.HandleFunc("/speed.json", r.speedJSON)

	if r.options.HTTPMux != nil {
		r.options.HTTPMux(mux)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		r.statusMu.Lock()
		status := r.lastStatus
		r.statusMu.Unlock()

		if r.options.StatusPage != nil {
			_, _ = io.WriteString(w, r.options.StatusPage(status, formatLimit(r.limiter.Limit())))

			return
		}

		_, _ = fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><title>%s</title>%s
<body style="font-family:monospace">
<pre id=progress>%s</pre>
<p id=limit>rate limit: %s lines/sec</p>
<form method="post" action="/rate-limit">
<input name="value" placeholder="lines/sec, 0 or less = unlimited">
<button type="submit">set</button>
</form>
<div id=lps style="display:none"></div>
<div id=mbps></div>
<script>
// Poll the page and update only the read-only parts, so the form is never overwritten mid-edit.
setInterval(async () => {
  try {
    const r = await fetch(location.href, {cache: 'no-store'});
    if (!r.ok) return;
    const doc = new DOMParser().parseFromString(await r.text(), 'text/html');
    document.title = doc.title;
    for (const id of ['progress', 'limit']) document.getElementById(id).innerHTML = doc.getElementById(id).innerHTML;
  } catch (e) {}
}, 1000);

// Charts get new points in place, the first fetch draws them.
let since = 0;
const charts = [['lps', 'Lines per second', 1], ['mbps', 'MB per second', 2]];
setInterval(async () => {
  try {
    const rows = await (await fetch('/speed.json?since=' + since, {cache: 'no-store'})).json();
    if (!rows.length) return;
    for (const [id, caption, col] of charts) {
      const pts = rows.map(r => [r[0], r[col]]);
      if (id === 'lps') {
        if (!pts.some(p => p[1] > 0) && !since) continue;
        document.getElementById(id).style.display = '';
      }
      if (!since) Plot.replace(id, {columns: ['time', caption], values: pts}, {time: true, caption: caption, height: 200});
      else if (document.getElementById(id).style.display !== 'none') Plot.append(id, pts);
    }
    since = rows[rows.length - 1][0];
  } catch (e) {}
}, 1000);
</script>
</body>`, html.EscapeString(pageTitle(status)), plot.HeadTags("/plot/"), html.EscapeString(status), formatLimit(r.limiter.Limit()))
	})

	var handler http.Handler = mux

	if r.httpAuth != "" {
		user, pass, _ := strings.Cut(r.httpAuth, ":")
		handler = basicAuth(user, pass, mux)
	}

	go func() {
		if err := http.Serve(ln, handler); err != nil { //nolint:gosec
			log.Println("control server stopped:", err.Error())
		}
	}()

	return nil
}

func basicAuth(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		u, p, ok := req.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="catp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(w, req)
	})
}

func (r *runner) cat(filename string) error {
	return r.catChunk(filename, nil)
}

// catChunk processes a file, or a byte range of it if c is not nil, in which case filename is
// the virtual name of the chunk.
func (r *runner) catChunk(filename string, c *chunk) (err error) {
	if r.samples > 0 {
		handled, reason, err := r.trySampleSeekable(filename)
		if handled {
			return err
		}

		if _, err := fmt.Fprintf(os.Stderr, "%s: full scan is used to collect samples because %s\n", filename, reason); err != nil {
			return err
		}
	}

	var rd io.Reader

	if filename == "-" {
		rd = os.Stdin
	} else {
		path := filename
		if c != nil {
			path = c.path
		}

		file, err := os.Open(path) //nolint:gosec
		if err != nil {
			return err
		}

		defer func() {
			if clErr := file.Close(); clErr != nil && err == nil {
				err = clErr
			}
		}()

		rd = io.Reader(file)

		if c != nil {
			if rd, err = newChunkReader(file, c.start, c.end); err != nil {
				return err
			}
		}
	}

	// sampleBytes tracks compressed bytes read for this file only, independent of the shared
	// (and -no-progress-gated) progress counters below, so -samples can gate evenly spaced lines
	// through a full scan regardless of -no-progress or -parallel.
	var sampleBytes *int64

	if r.samples > 0 {
		sampleBytes = new(int64)

		scr := progress.NewCountingReader(rd)
		scr.SetBytes(sampleBytes)
		scr.SetLines(nil)

		rd = scr
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

		w, closer, err := makeWriter(fn)
		if err != nil {
			return err
		}

		defer func() {
			if err := closer(); err != nil {
				log.Println("failed to close writer:", err.Error())
			}
		}()

		out = w
	}

	if r.parallel <= 1 && !r.noProgress && !r.hasCompression {
		r.consumed = new(int64)
	}

	if r.parallel <= 1 && !r.noProgress {
		r.pr.Start(func(t *progress.Task) {
			t.TotalBytes = func() int64 {
				return r.totalBytes
			}

			t.CurrentBytes = r.currentFile.Bytes

			// Counters of this file, Progress adds counters of previous files (Continue) on its own.
			if consumed := r.consumed; consumed != nil {
				t.CurrentBytes = func() int64 { return atomic.LoadInt64(consumed) }
			}

			linesBase := atomic.LoadInt64(&r.currentLines)
			t.CurrentLines = func() int64 { return atomic.LoadInt64(&r.currentLines) - linesBase }
			t.Task = filename
			t.Continue = true
			t.PrintOnStart = true
		})
	}

	if r.startLine != 0 || r.endLine != 0 {
		r.countLines = true
	}

	if r.filters.isSet() || r.parallel > 1 || r.hasOptions || r.countLines || r.limiter != nil || sampleBytes != nil {
		r.scanFile(filename, rd, out, sampleBytes)
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

// Options allows behavior customizations.
type Options struct {
	// PrepareLine is invoked for every line, if result is nil, line is skipped.
	// You can use buf to avoid allocations for a result, and change its capacity if needed.
	PrepareLine func(filename string, lineNr int, line []byte, buf *[]byte) []byte

	// HTTPMux registers extra routes on the -http control server.
	HTTPMux func(mux *http.ServeMux)

	// StatusPage replaces the -http status page, it receives plain text progress status (not HTML-escaped)
	// and current rate limit, and returns a complete HTML page.
	StatusPage func(status, rateLimit string) string

	// VersionLabel is added to version message.
	VersionLabel string
}

func (r *runner) loadCSVFilter(fn string, pass bool) error {
	f, err := os.Open(fn) //nolint:gosec
	if err != nil {
		return fmt.Errorf("failed to open CSV file %s: %w", fn, err)
	}

	cr := csv.NewReader(f)

	rows, err := cr.ReadAll()
	if err != nil {
		return fmt.Errorf("failed to read CSV file %s: %w", fn, err)
	}

	for _, row := range rows {
		var and [][]byte
		for _, v := range row {
			and = append(and, []byte(v))
		}

		r.filters.addFilter(pass, and...)
	}

	return nil
}
