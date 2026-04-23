// Per-tenant Prometheus-compatible metrics.
//
// Rather than pulling in prometheus/client_golang — which would add a
// transitive dependency surface we do not otherwise need — this file
// hand-rolls the bits of the exposition format that matter: a counter
// vector, a histogram vector (with fixed buckets), and a gauge vector,
// each keyed by tenant_id + arbitrary label set. The exposition output
// is Prometheus text format 0.0.4 so scrapers and Grafana Agent work
// unmodified.
//
// The middleware measures request_duration_seconds and request_total
// with labels {tenant_id, method, path, status}. `path` is the chi
// route pattern, not the raw URL, so high-cardinality IDs do not blow
// up the cardinality budget.
package platform

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
)

// DefaultDurationBuckets is a balanced latency histogram for both the
// fast (<1ms) control-plane paths and the slow (~1s) import / report
// paths. Keeping the bucket list small keeps the series count per
// histogram bounded.
var DefaultDurationBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025,
	0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0,
}

// MetricsRegistry is the central metric store. Each metric family is
// append-only; individual series are created on first use. The
// registry is safe for concurrent use — the hot path on every request
// is (a) hashing the label values into a string key and (b) an atomic
// add under a read-locked family map.
type MetricsRegistry struct {
	mu         sync.RWMutex
	counters   map[string]*counterVec
	histograms map[string]*histogramVec
	gauges     map[string]*gaugeVec
}

// NewMetricsRegistry returns an empty registry.
func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{
		counters:   map[string]*counterVec{},
		histograms: map[string]*histogramVec{},
		gauges:     map[string]*gaugeVec{},
	}
}

// Counter returns (creating if needed) a counter vector with the given
// name and label keys. The help text is only used on first creation.
func (r *MetricsRegistry) Counter(name, help string, labelKeys ...string) *counterVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &counterVec{name: name, help: help, labelKeys: append([]string{}, labelKeys...), series: map[string]*counterSeries{}}
	r.counters[name] = c
	return c
}

// Histogram returns (creating if needed) a histogram vector with the
// given name, buckets (upper bounds in seconds), and label keys.
func (r *MetricsRegistry) Histogram(name, help string, buckets []float64, labelKeys ...string) *histogramVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.histograms[name]; ok {
		return h
	}
	if len(buckets) == 0 {
		buckets = DefaultDurationBuckets
	}
	h := &histogramVec{name: name, help: help, buckets: append([]float64{}, buckets...), labelKeys: append([]string{}, labelKeys...), series: map[string]*histogramSeries{}}
	r.histograms[name] = h
	return h
}

// Gauge returns (creating if needed) a gauge vector.
func (r *MetricsRegistry) Gauge(name, help string, labelKeys ...string) *gaugeVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := &gaugeVec{name: name, help: help, labelKeys: append([]string{}, labelKeys...), series: map[string]*gaugeSeries{}}
	r.gauges[name] = g
	return g
}

// Handler returns an http.HandlerFunc that writes the registry in
// Prometheus text exposition format 0.0.4.
func (r *MetricsRegistry) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.mu.RLock()
		defer r.mu.RUnlock()
		for _, name := range sortedKeys(r.counters) {
			r.counters[name].write(w)
		}
		for _, name := range sortedKeys(r.histograms) {
			r.histograms[name].write(w)
		}
		for _, name := range sortedKeys(r.gauges) {
			r.gauges[name].write(w)
		}
	}
}

type counterVec struct {
	name      string
	help      string
	labelKeys []string
	mu        sync.RWMutex
	series    map[string]*counterSeries
}

type counterSeries struct {
	labels map[string]string
	value  uint64
}

// Inc adds 1 to the counter with the supplied label values (matched
// positionally against labelKeys).
func (c *counterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds v to the counter with the supplied label values.
func (c *counterVec) Add(v uint64, labelValues ...string) {
	if len(labelValues) != len(c.labelKeys) {
		return
	}
	key := joinLabels(labelValues)
	c.mu.RLock()
	s, ok := c.series[key]
	c.mu.RUnlock()
	if !ok {
		s = &counterSeries{labels: labelMap(c.labelKeys, labelValues)}
		c.mu.Lock()
		if existing, dup := c.series[key]; dup {
			s = existing
		} else {
			c.series[key] = s
		}
		c.mu.Unlock()
	}
	atomic.AddUint64(&s.value, v)
}

func (c *counterVec) write(w http.ResponseWriter) {
	fprintf(w, "# HELP %s %s\n", c.name, c.help)
	fprintf(w, "# TYPE %s counter\n", c.name)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, key := range sortedKeys(c.series) {
		s := c.series[key]
		fprintf(w, "%s%s %d\n", c.name, formatLabels(s.labels), atomic.LoadUint64(&s.value))
	}
}

type histogramVec struct {
	name      string
	help      string
	buckets   []float64
	labelKeys []string
	mu        sync.RWMutex
	series    map[string]*histogramSeries
}

type histogramSeries struct {
	labels   map[string]string
	buckets  []uint64 // same length as vec.buckets
	countN   uint64
	sumBits  uint64 // float64 bits
	writeMux sync.Mutex
}

// Observe records one measurement into the histogram.
func (h *histogramVec) Observe(v float64, labelValues ...string) {
	if len(labelValues) != len(h.labelKeys) {
		return
	}
	key := joinLabels(labelValues)
	h.mu.RLock()
	s, ok := h.series[key]
	h.mu.RUnlock()
	if !ok {
		s = &histogramSeries{labels: labelMap(h.labelKeys, labelValues), buckets: make([]uint64, len(h.buckets))}
		h.mu.Lock()
		if existing, dup := h.series[key]; dup {
			s = existing
		} else {
			h.series[key] = s
		}
		h.mu.Unlock()
	}
	for i, ub := range h.buckets {
		if v <= ub {
			atomic.AddUint64(&s.buckets[i], 1)
		}
	}
	atomic.AddUint64(&s.countN, 1)
	// Sum requires a write lock because float64 doesn't have lock-free add
	s.writeMux.Lock()
	cur := float64FromBits(atomic.LoadUint64(&s.sumBits))
	atomic.StoreUint64(&s.sumBits, float64ToBits(cur+v))
	s.writeMux.Unlock()
}

func (h *histogramVec) write(w http.ResponseWriter) {
	fprintf(w, "# HELP %s %s\n", h.name, h.help)
	fprintf(w, "# TYPE %s histogram\n", h.name)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, key := range sortedKeys(h.series) {
		s := h.series[key]
		for i, ub := range h.buckets {
			labels := cloneAdd(s.labels, "le", strconv.FormatFloat(ub, 'g', -1, 64))
			fprintf(w, "%s_bucket%s %d\n", h.name, formatLabels(labels), atomic.LoadUint64(&s.buckets[i]))
		}
		infLabels := cloneAdd(s.labels, "le", "+Inf")
		fprintf(w, "%s_bucket%s %d\n", h.name, formatLabels(infLabels), atomic.LoadUint64(&s.countN))
		fprintf(w, "%s_sum%s %g\n", h.name, formatLabels(s.labels), float64FromBits(atomic.LoadUint64(&s.sumBits)))
		fprintf(w, "%s_count%s %d\n", h.name, formatLabels(s.labels), atomic.LoadUint64(&s.countN))
	}
}

type gaugeVec struct {
	name      string
	help      string
	labelKeys []string
	mu        sync.RWMutex
	series    map[string]*gaugeSeries
}

type gaugeSeries struct {
	labels map[string]string
	bits   uint64
}

// Set writes v into the gauge.
func (g *gaugeVec) Set(v float64, labelValues ...string) {
	if len(labelValues) != len(g.labelKeys) {
		return
	}
	key := joinLabels(labelValues)
	g.mu.RLock()
	s, ok := g.series[key]
	g.mu.RUnlock()
	if !ok {
		s = &gaugeSeries{labels: labelMap(g.labelKeys, labelValues)}
		g.mu.Lock()
		if existing, dup := g.series[key]; dup {
			s = existing
		} else {
			g.series[key] = s
		}
		g.mu.Unlock()
	}
	atomic.StoreUint64(&s.bits, float64ToBits(v))
}

func (g *gaugeVec) write(w http.ResponseWriter) {
	fprintf(w, "# HELP %s %s\n", g.name, g.help)
	fprintf(w, "# TYPE %s gauge\n", g.name)
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, key := range sortedKeys(g.series) {
		s := g.series[key]
		fprintf(w, "%s%s %g\n", g.name, formatLabels(s.labels), float64FromBits(atomic.LoadUint64(&s.bits)))
	}
}

// MetricsMiddleware wraps every request so request_total and
// request_duration_seconds are emitted with tenant_id, method, path,
// status labels. It must run inside TenantMiddleware so the tenant is
// on the context; missing tenants are reported as "anonymous" which
// keeps unauthenticated /health and /metrics hits observable without
// inflating cardinality.
func MetricsMiddleware(reg *MetricsRegistry) func(http.Handler) http.Handler {
	total := reg.Counter("kapp_request_total", "Total HTTP requests handled.", "tenant_id", "method", "path", "status")
	dur := reg.Histogram("kapp_request_duration_seconds", "HTTP request latency in seconds.", DefaultDurationBuckets, "tenant_id", "method", "path", "status")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			tenantID := "anonymous"
			if t := TenantFromContext(r.Context()); t != nil {
				tenantID = t.ID.String()
			}
			path := chi.RouteContext(r.Context()).RoutePattern()
			if path == "" {
				path = r.URL.Path
			}
			status := strconv.Itoa(sw.status)
			total.Inc(tenantID, r.Method, path, status)
			dur.Observe(time.Since(start).Seconds(), tenantID, r.Method, path, status)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	wrote   bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// --- label helpers --------------------------------------------------

func joinLabels(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.Join(values, "\x1f")
}

func labelMap(keys, values []string) map[string]string {
	m := make(map[string]string, len(keys))
	for i, k := range keys {
		m[k] = values[i]
	}
	return m
}

func cloneAdd(src map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(src)+1)
	for mk, mv := range src {
		out[mk] = mv
	}
	out[k] = v
	return out
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, escapeLabelValue(labels[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func float64ToBits(v float64) uint64   { return math.Float64bits(v) }
func float64FromBits(u uint64) float64 { return math.Float64frombits(u) }

// fprintf wraps fmt.Fprintf and drops the returned byte count /
// error. The Prometheus exposition format is best-effort: if the
// client disconnects mid-scrape we simply stop writing — no retry,
// no logging. Dedicating a helper keeps errcheck happy without
// sprinkling `_, _ =` everywhere.
func fprintf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
