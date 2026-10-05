package metrics

import (
	"strings"
	"testing"
)

func TestTextExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("t_requests_total", "Requests.", "route", "status")
	c.Inc("GET /v1/x", "200")
	c.Add(2, "GET /v1/x", "200")
	c.Inc(`we"ird\`, "500")
	g := r.NewGauge("t_active", "Active things.")
	g.Set(3)
	g.Add(-1)
	r.NewCounter("t_untouched_total", "Never incremented.")
	h := r.NewHistogram("t_duration_seconds", "Durations.", []float64{0.1, 1}, "route")
	h.Observe(0.05, "a")
	h.Observe(0.5, "a")
	h.Observe(5, "a")
	r.NewGaugeFunc("t_pool", "From a function.", func() float64 { return 7 })

	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# TYPE t_requests_total counter\n",
		`t_requests_total{route="GET /v1/x",status="200"} 3` + "\n",
		`t_requests_total{route="we\"ird\\",status="500"} 1` + "\n",
		"# TYPE t_active gauge\nt_active 2\n",
		"t_untouched_total 0\n",
		`t_duration_seconds_bucket{route="a",le="0.1"} 1` + "\n",
		`t_duration_seconds_bucket{route="a",le="1"} 2` + "\n",
		`t_duration_seconds_bucket{route="a",le="+Inf"} 3` + "\n",
		`t_duration_seconds_sum{route="a"} 5.55` + "\n",
		`t_duration_seconds_count{route="a"} 3` + "\n",
		"t_pool 7\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Sorted by name.
	if strings.Index(out, "t_active") > strings.Index(out, "t_requests_total") {
		t.Error("metrics are not sorted by name")
	}
	if c.Value("GET /v1/x", "200") != 3 || h.Count("a") != 3 {
		t.Error("accessors disagree with the exposition")
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("dup_total", "x")
	defer func() {
		if recover() == nil {
			t.Error("a duplicate metric name was accepted")
		}
	}()
	r.NewGauge("dup_total", "x")
}
