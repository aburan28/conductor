// Package metrics is a small Prometheus text-format registry (DESIGN.md §26.1).
//
// It implements the three shapes conductord needs — counters, gauges and fixed-bucket
// histograms, each optionally labelled — and the text exposition format, over the standard
// library. The official client would pull in a dependency tree larger than the rest of the
// server for what is a few hundred lines; this keeps §4's dependency posture.
//
// Label values must come from a bounded set (route patterns, status codes, event types),
// never from request data: every distinct label set is a series kept for the process's life.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds metrics and renders them.
type Registry struct {
	mu      sync.Mutex
	metrics map[string]metric
}

type metric interface {
	write(w *bufio.Writer)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{metrics: map[string]metric{}} }

// Default is the process-wide registry conductord serves at /metrics.
var Default = NewRegistry()

func (r *Registry) register(name string, m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.metrics[name]; dup {
		// Metrics are package-level variables; a duplicate is a programming error found at
		// init, not a runtime condition.
		panic("metrics: duplicate metric " + name)
	}
	r.metrics[name] = m
}

// WriteText renders every metric in the Prometheus text exposition format (version 0.0.4),
// sorted by name so successive scrapes diff cleanly.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	names := make([]string, 0, len(r.metrics))
	for n := range r.metrics {
		names = append(names, n)
	}
	sort.Strings(names)
	ms := make([]metric, len(names))
	for i, n := range names {
		ms[i] = r.metrics[n]
	}
	r.mu.Unlock()

	bw := bufio.NewWriter(w)
	for _, m := range ms {
		m.write(bw)
	}
	return bw.Flush()
}

// ContentType is the media type of WriteText's output.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// ---------------------------------------------------------------------------
// Labelled values
// ---------------------------------------------------------------------------

// series is the label-set → value store shared by counters and gauges.
type series struct {
	name, help, kind string
	labels           []string

	mu     sync.Mutex
	values map[string]float64
	keys   map[string][]string
}

func newSeries(name, help, kind string, labels []string) *series {
	return &series{name: name, help: help, kind: kind, labels: labels,
		values: map[string]float64{}, keys: map[string][]string{}}
}

func (s *series) key(values []string) string {
	if len(values) != len(s.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", s.name, len(s.labels), len(values)))
	}
	return strings.Join(values, "\xff")
}

func (s *series) add(v float64, values []string) {
	k := s.key(values)
	s.mu.Lock()
	if _, ok := s.keys[k]; !ok {
		s.keys[k] = append([]string(nil), values...)
	}
	s.values[k] += v
	s.mu.Unlock()
}

func (s *series) set(v float64, values []string) {
	k := s.key(values)
	s.mu.Lock()
	if _, ok := s.keys[k]; !ok {
		s.keys[k] = append([]string(nil), values...)
	}
	s.values[k] = v
	s.mu.Unlock()
}

func (s *series) write(w *bufio.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	header(w, s.name, s.help, s.kind)
	if len(s.labels) == 0 && len(s.values) == 0 {
		// An unlabelled metric is always present, at zero until first touched, so a rate()
		// over it has a starting point.
		fmt.Fprintf(w, "%s 0\n", s.name)
		return
	}
	for _, k := range sortedKeys(s.values) {
		fmt.Fprintf(w, "%s%s %s\n", s.name, labelString(s.labels, s.keys[k], "", ""), formatFloat(s.values[k]))
	}
}

// Counter only goes up.
type Counter struct{ s *series }

// NewCounter registers a counter.
func (r *Registry) NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{newSeries(name, help, "counter", labels)}
	r.register(name, c.s)
	return c
}

// Inc adds one.
func (c *Counter) Inc(labelValues ...string) { c.s.add(1, labelValues) }

// Add adds v, which must not be negative.
func (c *Counter) Add(v float64, labelValues ...string) {
	if v < 0 {
		return
	}
	c.s.add(v, labelValues)
}

// Value returns the current value for a label set, for tests.
func (c *Counter) Value(labelValues ...string) float64 {
	k := c.s.key(labelValues)
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	return c.s.values[k]
}

// Gauge goes up and down.
type Gauge struct{ s *series }

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string, labels ...string) *Gauge {
	g := &Gauge{newSeries(name, help, "gauge", labels)}
	r.register(name, g.s)
	return g
}

// Set replaces the value.
func (g *Gauge) Set(v float64, labelValues ...string) { g.s.set(v, labelValues) }

// Add adds v (which may be negative).
func (g *Gauge) Add(v float64, labelValues ...string) { g.s.add(v, labelValues) }

// Value returns the current value for a label set, for tests.
func (g *Gauge) Value(labelValues ...string) float64 {
	k := g.s.key(labelValues)
	g.s.mu.Lock()
	defer g.s.mu.Unlock()
	return g.s.values[k]
}

// funcMetric reads its value at scrape time, for figures something else already keeps (the
// database pool's statistics).
type funcMetric struct {
	name, help, kind string
	fn               func() float64
}

func (f *funcMetric) write(w *bufio.Writer) {
	header(w, f.name, f.help, f.kind)
	fmt.Fprintf(w, "%s %s\n", f.name, formatFloat(f.fn()))
}

// NewGaugeFunc registers a gauge read from fn at scrape time.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) {
	r.register(name, &funcMetric{name, help, "gauge", fn})
}

// NewCounterFunc registers a counter read from fn at scrape time; fn must never decrease.
func (r *Registry) NewCounterFunc(name, help string, fn func() float64) {
	r.register(name, &funcMetric{name, help, "counter", fn})
}

// ---------------------------------------------------------------------------
// Histograms
// ---------------------------------------------------------------------------

// DurationBuckets suit request and tick durations in seconds.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Histogram counts observations into fixed buckets.
type Histogram struct {
	name, help string
	labels     []string
	buckets    []float64

	mu   sync.Mutex
	data map[string]*histData
}

type histData struct {
	labels []string
	counts []uint64 // per bucket, not cumulative
	count  uint64
	sum    float64
}

// NewHistogram registers a histogram. buckets must be sorted ascending.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	h := &Histogram{name: name, help: help, labels: labels, buckets: buckets, data: map[string]*histData{}}
	r.register(name, h)
	return h
}

// Observe records one value.
func (h *Histogram) Observe(v float64, labelValues ...string) {
	if len(labelValues) != len(h.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", h.name, len(h.labels), len(labelValues)))
	}
	k := strings.Join(labelValues, "\xff")
	i := sort.SearchFloat64s(h.buckets, v) // first bucket with bound >= v
	h.mu.Lock()
	d, ok := h.data[k]
	if !ok {
		d = &histData{labels: append([]string(nil), labelValues...), counts: make([]uint64, len(h.buckets))}
		h.data[k] = d
	}
	if i < len(h.buckets) {
		d.counts[i]++
	}
	d.count++
	d.sum += v
	h.mu.Unlock()
}

// Count returns how many values were observed for a label set, for tests.
func (h *Histogram) Count(labelValues ...string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d, ok := h.data[strings.Join(labelValues, "\xff")]; ok {
		return d.count
	}
	return 0
}

func (h *Histogram) write(w *bufio.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	header(w, h.name, h.help, "histogram")
	keys := make([]string, 0, len(h.data))
	for k := range h.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := h.data[k]
		var cum uint64
		for i, b := range h.buckets {
			cum += d.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(h.labels, d.labels, "le", formatFloat(b)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(h.labels, d.labels, "le", "+Inf"), d.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, labelString(h.labels, d.labels, "", ""), formatFloat(d.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, labelString(h.labels, d.labels, "", ""), d.count)
	}
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

func header(w *bufio.Writer, name, help, kind string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, kind)
}

func labelString(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(values[i]))
		b.WriteByte('"')
	}
	if extraName != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(extraName)
		b.WriteString(`="`)
		b.WriteString(extraValue)
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
