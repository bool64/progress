// Package progress provides helpers to print progress status.
package progress

import (
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
	"time"
)

// Status describes current progress.
type Status struct {
	Task           string        `json:"task"`
	DonePercent    float64       `json:"done_percent"`
	LinesCompleted int64         `json:"lines_completed"`
	BytesCompleted int64         `json:"bytes_completed"`
	SpeedMBPS      float64       `json:"speed_mbps"`
	SpeedLPS       float64       `json:"speed_lps"`
	Elapsed        time.Duration `json:"-"`
	Remaining      time.Duration `json:"-"`
	Metrics        []Metric      `json:"-"`
}

// Progress reports reading performance.
type Progress struct {
	Interval       time.Duration
	Print          func(status Status)
	ShowHeapStats  bool
	ShowLinesStats bool

	// IncrementalSpeed shows speed and remaining estimate based on performance between two status updates.
	IncrementalSpeed bool

	done    chan bool
	task    Task
	lines   func() int64
	current func() int64
	tot     func() int64
	prnt    func(s Status)
	start   time.Time

	prevStatus Status

	continuedLines int64
	continuedBytes int64
	metrics        []Metric
}

// Type describes metric value.
type Type string

// Type values.
const (
	Bytes    = Type("bytes")
	Duration = Type("duration")
	Gauge    = Type("gauge")
)

// Metric is an operation metric.
type Metric struct {
	Name  string
	Type  Type
	Value func() int64
}

// DefaultStatus renders Status as a string.
func DefaultStatus(s Status) string {
	if s.Task != "" {
		s.Task += ": "
	}

	ms := runtime.MemStats{}
	runtime.ReadMemStats(&ms)

	heapMB := ms.HeapInuse / (1024 * 1024)

	res := fmt.Sprintf(s.Task+"%.1f%% bytes read, %d lines processed, %.1f l/s, %.1f MB/s, elapsed %s, remaining %s, heap %d MB",
		s.DonePercent, s.LinesCompleted, s.SpeedLPS, s.SpeedMBPS,
		s.Elapsed.Round(10*time.Millisecond).String(), s.Remaining.String(), heapMB)

	return res
}

// MetricsStatus renders Status metrics as a string.
func MetricsStatus(s Status) string {
	metrics := ""

	for _, m := range s.Metrics {
		switch m.Type {
		case Bytes:
			spdMBPS := float64(m.Value()) / (s.Elapsed.Seconds() * 1024 * 1024)
			metrics += fmt.Sprintf("%s: %.1f MB/s, ", m.Name, spdMBPS)
		case Duration:
			metrics += m.Name + ": " + time.Duration(m.Value()).String() + ", "
		case Gauge:
			metrics += fmt.Sprintf("%s: %d, ", m.Name, m.Value())
		}
	}

	if metrics != "" {
		metrics = metrics[:len(metrics)-2]
	}

	return metrics
}

// Task describes a long-running process.
type Task struct {
	TotalBytes   func() int64
	CurrentBytes func() int64
	CurrentLines func() int64
	Task         string
	Continue     bool
	PrintOnStart bool
}

// Start spawns background progress reporter.
func (p *Progress) Start(options ...func(t *Task)) {
	p.done = make(chan bool)

	task := Task{}
	for _, o := range options {
		o(&task)
	}

	if task.Continue {
		if p.current != nil {
			p.continuedBytes += p.current()
		}

		if p.lines != nil {
			p.continuedLines += p.lines()
		}
	}

	p.task = task
	p.current = task.CurrentBytes
	p.lines = task.CurrentLines
	p.tot = task.TotalBytes

	interval := p.Interval
	if interval == 0 {
		interval = time.Second
	}

	p.prnt = p.Print
	if p.prnt == nil {
		p.prnt = func(s Status) {
			println(DefaultStatus(s))
		}
	}

	if !task.Continue || p.start.IsZero() {
		p.start = time.Now()
		p.continuedBytes = 0
		p.continuedLines = 0
	}

	p.startPrinter(interval)
}

func (p *Progress) startPrinter(interval time.Duration) {
	done := p.done
	t := time.NewTicker(interval)

	go func() {
		for {
			select {
			case <-t.C:
				p.printStatus(false)

			case <-done:
				t.Stop()

				return
			}
		}
	}()

	if p.task.PrintOnStart {
		go func() {
			time.Sleep(time.Millisecond)
			p.printStatus(false)
		}()
	}
}

// AddMetrics adds more metrics to progress status message.
func (p *Progress) AddMetrics(metrics ...Metric) {
	p.metrics = append(p.metrics, metrics...)
}

// Reset drops continued counters.
func (p *Progress) Reset() {
	p.start = time.Time{}
	p.continuedLines = 0
	p.continuedBytes = 0
	p.metrics = nil
}

func (p *Progress) speedStatus(s *Status) {
	if !p.IncrementalSpeed {
		s.SpeedMBPS = (float64(s.BytesCompleted) / s.Elapsed.Seconds()) / (1024 * 1024)
		s.SpeedLPS = float64(s.LinesCompleted) / s.Elapsed.Seconds()

		if s.DonePercent > 0 {
			s.Remaining = time.Duration(float64(100*s.Elapsed)/s.DonePercent) - s.Elapsed
			s.Remaining = s.Remaining.Truncate(time.Second)
		} else {
			s.Remaining = 0
		}

		return
	}

	lc := s.LinesCompleted - p.prevStatus.LinesCompleted
	bc := s.BytesCompleted - p.prevStatus.BytesCompleted
	dc := s.DonePercent - p.prevStatus.DonePercent
	ela := s.Elapsed - p.prevStatus.Elapsed

	if ela != 0 {
		if lc > 0 {
			s.SpeedLPS = float64(lc) / ela.Seconds()
		}

		if bc > 0 {
			s.SpeedMBPS = (float64(bc) / ela.Seconds()) / (1024 * 1024)
		}
	}

	if dc > 0 {
		s.Remaining = time.Duration((100.0 - s.DonePercent) * float64(ela) / dc)
		s.Remaining = s.Remaining.Truncate(time.Second)
	} else {
		s.Remaining = 0
	}

	p.prevStatus = *s
}

func (p *Progress) printStatus(last bool) {
	s := Status{}
	s.Task = p.task.Task
	s.LinesCompleted = p.Lines()
	s.BytesCompleted = p.Bytes()
	s.Metrics = p.metrics
	s.Elapsed = time.Since(p.start)
	s.DonePercent = 100 * float64(s.BytesCompleted) / float64(p.tot())

	p.speedStatus(&s)

	if s.Remaining > 100*time.Millisecond || s.Remaining == 0 || last {
		p.prnt(s)
	}
}

// Stop stops progress reporting.
func (p *Progress) Stop() {
	p.printStatus(true)

	if !p.task.Continue {
		p.metrics = nil
	}

	close(p.done)
}

// Bytes returns current number of bytes.
func (p *Progress) Bytes() int64 {
	if p.current != nil {
		return p.continuedBytes + p.current()
	}

	return p.continuedBytes
}

// Lines returns current number of lines.
func (p *Progress) Lines() int64 {
	if p.lines != nil {
		return p.continuedLines + p.lines()
	}

	return p.continuedLines
}

// NewCountingReader wraps an io.Reader with counters of bytes and lines.
func NewCountingReader(r io.Reader) *CountingReader {
	cr := &CountingReader{
		Reader: r,
	}
	cr.lines = new(int64)
	cr.bytes = new(int64)

	return cr
}

// CountingReader wraps io.Reader to count bytes.
type CountingReader struct {
	Reader io.Reader
	sharedCounters
}

type sharedCounters struct {
	lines *int64
	bytes *int64

	localBytes int64
	localLines int64
}

func (cr *sharedCounters) SetLines(lines *int64) {
	cr.lines = lines
}

func (cr *sharedCounters) SetBytes(bytes *int64) {
	cr.bytes = bytes
}

func (cr *sharedCounters) count(n int, p []byte, err error) {
	cr.localBytes += int64(n)

	if (err != nil || cr.localBytes > 100000) && cr.bytes != nil {
		atomic.AddInt64(cr.bytes, cr.localBytes)
		cr.localBytes = 0
	}

	if cr.lines == nil {
		return
	}

	for i := 0; i < n; i++ {
		if p[i] == '\n' {
			cr.localLines++
		}
	}

	if err != nil || cr.localLines > 1000 {
		atomic.AddInt64(cr.lines, cr.localLines)
		cr.localLines = 0
	}
}

// Read reads and counts bytes.
func (cr *CountingReader) Read(p []byte) (n int, err error) {
	n, err = cr.Reader.Read(p)
	cr.count(n, p, err)

	return n, err
}

func (cr *sharedCounters) Close() {
	if cr.localBytes > 0 && cr.bytes != nil {
		atomic.AddInt64(cr.bytes, cr.localBytes)
		cr.localBytes = 0
	}

	if cr.localLines > 0 && cr.lines != nil {
		atomic.AddInt64(cr.lines, cr.localLines)
		cr.localLines = 0
	}
}

// Bytes returns number of processed bytes.
func (cr *sharedCounters) Bytes() int64 {
	return atomic.LoadInt64(cr.bytes)
}

// Lines returns number of processed lines.
func (cr *sharedCounters) Lines() int64 {
	return atomic.LoadInt64(cr.lines)
}

// NewCountingWriter wraps an io.Writer with counters of bytes and lines.
func NewCountingWriter(w io.Writer) *CountingWriter {
	cw := &CountingWriter{Writer: w}
	cw.lines = new(int64)
	cw.bytes = new(int64)

	return cw
}

// CountingWriter wraps io.Writer to count bytes.
type CountingWriter struct {
	Writer io.Writer
	sharedCounters
}

// Write writes and counts bytes.
func (cr *CountingWriter) Write(p []byte) (n int, err error) {
	n, err = cr.Writer.Write(p)
	cr.count(n, p, err)

	return n, err
}

// MetricsExposer provides metric counters.
type MetricsExposer interface {
	Metrics() []Metric
}
