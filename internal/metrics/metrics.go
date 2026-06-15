// Package metrics is a tiny, dependency-free metrics registry that renders the
// Prometheus text exposition format. It deliberately avoids
// prometheus/client_golang so the server stays a single pure-Go binary with a
// minimal dependency tree. It supports counters and histograms (optionally with
// labels) that the application updates directly, plus function-backed counters
// and gauges that read a live value at scrape time (used to surface counters
// that already exist as atomics elsewhere without double-bookkeeping).
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds a set of collectors and renders them in registration order.
type Registry struct {
	mu         sync.Mutex
	collectors []collector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// collector is anything that can write its exposition lines.
type collector interface {
	write(w io.Writer) error
}

func (r *Registry) register(c collector) {
	r.mu.Lock()
	r.collectors = append(r.collectors, c)
	r.mu.Unlock()
}

// WriteProm writes the full exposition for every registered collector.
func (r *Registry) WriteProm(w io.Writer) error {
	r.mu.Lock()
	cs := make([]collector, len(r.collectors))
	copy(cs, r.collectors)
	r.mu.Unlock()
	for _, c := range cs {
		if err := c.write(w); err != nil {
			return err
		}
	}
	return nil
}

// --- counters -------------------------------------------------------------

// NewCounter registers a counter family with the given label names (zero or
// more) and returns the vector used to obtain per-label-set counters.
func (r *Registry) NewCounter(name, help string, labelNames ...string) *CounterVec {
	v := &CounterVec{name: name, help: help, labelNames: labelNames, children: map[string]*Counter{}}
	r.register(v)
	return v
}

// CounterVec is a family of counters partitioned by label values.
type CounterVec struct {
	name, help string
	labelNames []string
	mu         sync.Mutex
	children   map[string]*Counter
}

// With returns the counter for the given label values (created on first use).
// The number of values must match the number of label names.
func (v *CounterVec) With(labelValues ...string) *Counter {
	if len(labelValues) != len(v.labelNames) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", v.name, len(v.labelNames), len(labelValues)))
	}
	key := strings.Join(labelValues, "\x00")
	v.mu.Lock()
	defer v.mu.Unlock()
	c, ok := v.children[key]
	if !ok {
		c = &Counter{labelValues: append([]string(nil), labelValues...)}
		v.children[key] = c
	}
	return c
}

func (v *CounterVec) write(w io.Writer) error {
	if err := writeHelpType(w, v.name, "counter", v.help); err != nil {
		return err
	}
	v.mu.Lock()
	lines := make([]string, 0, len(v.children))
	for _, c := range v.children {
		lines = append(lines, v.name+labelsString(v.labelNames, c.labelValues, "", "")+" "+formatFloat(c.value()))
	}
	v.mu.Unlock()
	sort.Strings(lines)
	return writeLines(w, lines)
}

// Counter is a single monotonically increasing value.
type Counter struct {
	bits        atomic.Uint64 // float64 bits
	labelValues []string
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add increases the counter by delta, which must be non-negative.
func (c *Counter) Add(delta float64) {
	if delta < 0 {
		panic("metrics: Counter.Add with negative delta")
	}
	for {
		old := c.bits.Load()
		nv := math.Float64frombits(old) + delta
		if c.bits.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

func (c *Counter) value() float64 { return math.Float64frombits(c.bits.Load()) }

// --- gauges ---------------------------------------------------------------

// NewGauge registers a gauge family with the given label names (zero or more).
func (r *Registry) NewGauge(name, help string, labelNames ...string) *GaugeVec {
	v := &GaugeVec{name: name, help: help, labelNames: labelNames, children: map[string]*Gauge{}}
	r.register(v)
	return v
}

// GaugeVec is a family of gauges partitioned by label values.
type GaugeVec struct {
	name, help string
	labelNames []string
	mu         sync.Mutex
	children   map[string]*Gauge
}

// With returns the gauge for the given label values (created on first use).
func (v *GaugeVec) With(labelValues ...string) *Gauge {
	if len(labelValues) != len(v.labelNames) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", v.name, len(v.labelNames), len(labelValues)))
	}
	key := strings.Join(labelValues, "\x00")
	v.mu.Lock()
	defer v.mu.Unlock()
	g, ok := v.children[key]
	if !ok {
		g = &Gauge{labelValues: append([]string(nil), labelValues...)}
		v.children[key] = g
	}
	return g
}

func (v *GaugeVec) write(w io.Writer) error {
	if err := writeHelpType(w, v.name, "gauge", v.help); err != nil {
		return err
	}
	v.mu.Lock()
	lines := make([]string, 0, len(v.children))
	for _, g := range v.children {
		lines = append(lines, v.name+labelsString(v.labelNames, g.labelValues, "", "")+" "+formatFloat(g.value()))
	}
	v.mu.Unlock()
	sort.Strings(lines)
	return writeLines(w, lines)
}

// Gauge is a single value that can go up or down.
type Gauge struct {
	bits        atomic.Uint64 // float64 bits
	labelValues []string
}

// Set replaces the gauge value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Inc adds one. Dec subtracts one. Add applies an arbitrary delta.
func (g *Gauge) Inc() { g.Add(1) }
func (g *Gauge) Dec() { g.Add(-1) }

// Add applies delta (which may be negative).
func (g *Gauge) Add(delta float64) {
	for {
		old := g.bits.Load()
		nv := math.Float64frombits(old) + delta
		if g.bits.CompareAndSwap(old, math.Float64bits(nv)) {
			return
		}
	}
}

func (g *Gauge) value() float64 { return math.Float64frombits(g.bits.Load()) }

// --- histograms -----------------------------------------------------------

// NewHistogram registers a histogram family. buckets are the upper bounds (a
// "+Inf" bucket is always added); they are sorted ascending. labelNames may be
// empty.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	bs := append([]float64(nil), buckets...)
	sort.Float64s(bs)
	v := &HistogramVec{name: name, help: help, buckets: bs, labelNames: labelNames, children: map[string]*Histogram{}}
	r.register(v)
	return v
}

// HistogramVec is a family of histograms partitioned by label values.
type HistogramVec struct {
	name, help string
	buckets    []float64
	labelNames []string
	mu         sync.Mutex
	children   map[string]*Histogram
}

// With returns the histogram for the given label values (created on first use).
func (v *HistogramVec) With(labelValues ...string) *Histogram {
	if len(labelValues) != len(v.labelNames) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", v.name, len(v.labelNames), len(labelValues)))
	}
	key := strings.Join(labelValues, "\x00")
	v.mu.Lock()
	defer v.mu.Unlock()
	h, ok := v.children[key]
	if !ok {
		h = &Histogram{buckets: v.buckets, counts: make([]atomic.Uint64, len(v.buckets)), labelValues: append([]string(nil), labelValues...)}
		v.children[key] = h
	}
	return h
}

func (v *HistogramVec) write(w io.Writer) error {
	if err := writeHelpType(w, v.name, "histogram", v.help); err != nil {
		return err
	}
	v.mu.Lock()
	children := make([]*Histogram, 0, len(v.children))
	for _, h := range v.children {
		children = append(children, h)
	}
	v.mu.Unlock()
	sort.Slice(children, func(i, j int) bool {
		return strings.Join(children[i].labelValues, "\x00") < strings.Join(children[j].labelValues, "\x00")
	})

	var lines []string
	for _, h := range children {
		base := labelsString(v.labelNames, h.labelValues, "", "")
		cum := uint64(0)
		for i, b := range v.buckets {
			cum += h.counts[i].Load()
			lines = append(lines, v.name+"_bucket"+withLE(v.labelNames, h.labelValues, formatFloat(b))+" "+strconv.FormatUint(cum, 10))
		}
		total := h.count.Load()
		lines = append(lines, v.name+"_bucket"+withLE(v.labelNames, h.labelValues, "+Inf")+" "+strconv.FormatUint(total, 10))
		lines = append(lines, v.name+"_sum"+base+" "+formatFloat(h.sum()))
		lines = append(lines, v.name+"_count"+base+" "+strconv.FormatUint(total, 10))
	}
	return writeLines(w, lines)
}

// Histogram observes float64 values into fixed buckets.
type Histogram struct {
	buckets     []float64
	counts      []atomic.Uint64 // per-bucket landing counts (non-cumulative)
	sumBits     atomic.Uint64   // float64 bits
	count       atomic.Uint64
	labelValues []string
}

// Observe records a single value.
func (h *Histogram) Observe(v float64) {
	h.count.Add(1)
	for {
		old := h.sumBits.Load()
		nv := math.Float64frombits(old) + v
		if h.sumBits.CompareAndSwap(old, math.Float64bits(nv)) {
			break
		}
	}
	// Land in the smallest bucket whose upper bound is >= v; values above the
	// largest finite bucket are counted only by the implicit +Inf bucket (the
	// total count).
	idx := sort.SearchFloat64s(h.buckets, v)
	if idx < len(h.buckets) {
		h.counts[idx].Add(1)
	}
}

func (h *Histogram) sum() float64 { return math.Float64frombits(h.sumBits.Load()) }

// --- function-backed collectors -------------------------------------------

// NewCounterFunc registers a label-less counter whose value is read from f at
// scrape time.
func (r *Registry) NewCounterFunc(name, help string, f func() float64) {
	r.register(&funcCollector{name: name, help: help, typ: "counter", f: f})
}

// NewGaugeFunc registers a label-less gauge whose value is read from f at
// scrape time.
func (r *Registry) NewGaugeFunc(name, help string, f func() float64) {
	r.register(&funcCollector{name: name, help: help, typ: "gauge", f: f})
}

type funcCollector struct {
	name, help, typ string
	f               func() float64
}

func (c *funcCollector) write(w io.Writer) error {
	if err := writeHelpType(w, c.name, c.typ, c.help); err != nil {
		return err
	}
	return writeLines(w, []string{c.name + " " + formatFloat(c.f())})
}

// --- formatting helpers ---------------------------------------------------

func writeHelpType(w io.Writer, name, typ, help string) error {
	_, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
	return err
}

func writeLines(w io.Writer, lines []string) error {
	for _, ln := range lines {
		if _, err := io.WriteString(w, ln+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// labelsString renders the {k="v",...} suffix with labels sorted by name. When
// there are no labels it returns "". extraName/extraVal append one more label
// (used for the histogram le label) when extraName != "".
func labelsString(names, values []string, extraName, extraVal string) string {
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(names)+1)
	for i, n := range names {
		pairs = append(pairs, kv{n, values[i]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })
	if extraName != "" {
		// le is conventionally rendered last on histogram bucket lines.
		pairs = append(pairs, kv{extraName, extraVal})
	}
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p.k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(p.v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func withLE(names, values []string, le string) string {
	return labelsString(names, values, "le", le)
}

func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
