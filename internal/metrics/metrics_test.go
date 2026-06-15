package metrics

import (
	"strings"
	"sync"
	"testing"
)

// render is a test helper that gathers the registry into Prometheus text.
func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WriteProm(&b); err != nil {
		t.Fatalf("WriteProm: %v", err)
	}
	return b.String()
}

// hasLine reports whether out contains the exact line (ignoring surrounding
// whitespace on each line).
func hasLine(out, want string) bool {
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == want {
			return true
		}
	}
	return false
}

func TestCounter_NoLabels(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("omnilog_test_total", "a test counter")
	c.With().Inc()
	c.With().Inc()
	c.With().Add(3)

	out := render(t, r)
	if !hasLine(out, "# HELP omnilog_test_total a test counter") {
		t.Errorf("missing HELP line in:\n%s", out)
	}
	if !hasLine(out, "# TYPE omnilog_test_total counter") {
		t.Errorf("missing TYPE line in:\n%s", out)
	}
	if !hasLine(out, "omnilog_test_total 5") {
		t.Errorf("missing value line in:\n%s", out)
	}
}

func TestCounterVec_LabelsSortedAndEscaped(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("omnilog_http_requests_total", "requests", "method", "code")
	c.With("GET", "200").Inc()
	c.With("GET", "500").Add(2)
	// A label value needing escaping.
	c.With(`a"b\c`, "200").Inc()

	out := render(t, r)
	// Labels must be rendered sorted by name (code before method).
	if !hasLine(out, `omnilog_http_requests_total{code="200",method="GET"} 1`) {
		t.Errorf("GET/200 line wrong:\n%s", out)
	}
	if !hasLine(out, `omnilog_http_requests_total{code="500",method="GET"} 2`) {
		t.Errorf("GET/500 line wrong:\n%s", out)
	}
	if !hasLine(out, `omnilog_http_requests_total{code="200",method="a\"b\\c"} 1`) {
		t.Errorf("escaped label line wrong:\n%s", out)
	}
}

func TestHistogram_BucketMath(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("omnilog_dur_seconds", "durations", []float64{1, 2, 5}, "op")
	for _, v := range []float64{0.5, 1.5, 3, 10} {
		h.With("search").Observe(v)
	}

	out := render(t, r)
	if !hasLine(out, "# TYPE omnilog_dur_seconds histogram") {
		t.Errorf("missing histogram TYPE:\n%s", out)
	}
	// Cumulative buckets: le=1 ->1, le=2 ->2, le=5 ->3, +Inf ->4.
	for _, want := range []string{
		`omnilog_dur_seconds_bucket{op="search",le="1"} 1`,
		`omnilog_dur_seconds_bucket{op="search",le="2"} 2`,
		`omnilog_dur_seconds_bucket{op="search",le="5"} 3`,
		`omnilog_dur_seconds_bucket{op="search",le="+Inf"} 4`,
		`omnilog_dur_seconds_sum{op="search"} 15`,
		`omnilog_dur_seconds_count{op="search"} 4`,
	} {
		if !hasLine(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestGaugeVec_SetIncDec(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("omnilog_build_info", "build info", "version")
	g.With("1.2.3").Set(1)

	gg := r.NewGauge("omnilog_inflight", "inflight")
	gg.With().Inc()
	gg.With().Inc()
	gg.With().Dec()

	out := render(t, r)
	if !hasLine(out, "# TYPE omnilog_build_info gauge") {
		t.Errorf("gauge TYPE missing:\n%s", out)
	}
	if !hasLine(out, `omnilog_build_info{version="1.2.3"} 1`) {
		t.Errorf("build_info line missing:\n%s", out)
	}
	if !hasLine(out, "omnilog_inflight 1") {
		t.Errorf("inflight gauge wrong (set/inc/dec):\n%s", out)
	}
}

func TestFuncCollectors(t *testing.T) {
	r := NewRegistry()
	r.NewCounterFunc("omnilog_ingest_received_total", "received", func() float64 { return 42 })
	r.NewGaugeFunc("omnilog_ingest_queued", "queued", func() float64 { return 7 })

	out := render(t, r)
	if !hasLine(out, "# TYPE omnilog_ingest_received_total counter") {
		t.Errorf("counterfunc TYPE missing:\n%s", out)
	}
	if !hasLine(out, "omnilog_ingest_received_total 42") {
		t.Errorf("counterfunc value missing:\n%s", out)
	}
	if !hasLine(out, "# TYPE omnilog_ingest_queued gauge") {
		t.Errorf("gaugefunc TYPE missing:\n%s", out)
	}
	if !hasLine(out, "omnilog_ingest_queued 7") {
		t.Errorf("gaugefunc value missing:\n%s", out)
	}
}

func TestWith_WrongArityPanics(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("omnilog_x_total", "x", "a", "b")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on wrong label arity")
		}
	}()
	c.With("only-one") // needs two values
}

func TestAdd_NegativePanics(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("omnilog_y_total", "y")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on negative Add")
		}
	}()
	c.With().Add(-1)
}

func TestConcurrent_IncAndObserve(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("omnilog_c_total", "c")
	h := r.NewHistogram("omnilog_h_seconds", "h", []float64{1, 10, 100})

	const goroutines, each = 8, 1000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c.With().Inc()
				h.With().Observe(5)
			}
		}()
	}
	wg.Wait()

	out := render(t, r)
	if !hasLine(out, "omnilog_c_total 8000") {
		t.Errorf("counter total wrong under concurrency:\n%s", out)
	}
	if !hasLine(out, "omnilog_h_seconds_count 8000") {
		t.Errorf("histogram count wrong under concurrency:\n%s", out)
	}
}
