package server

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const historySize = 100

type reqRecord struct {
	Time   string `json:"time"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Ms     int64  `json:"ms"`
}

// metrics tracks API traffic: lifetime counters plus a ring of recent
// requests for the console's history rail. Console-internal queries are
// never recorded; only real client traffic lands here.
type metrics struct {
	mu      sync.Mutex
	start   time.Time
	total   int64
	errors  int64
	totalMs int64
	byRoute map[string]int64
	recent  []reqRecord
}

func newMetrics() *metrics {
	return &metrics{start: time.Now(), byRoute: map[string]int64{}}
}

func (m *metrics) record(method, path string, status int, ms int64) {
	route := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0]
	if i := strings.IndexByte(route, '?'); i >= 0 {
		route = route[:i]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total++
	m.totalMs += ms
	if status >= 400 {
		m.errors++
	}
	m.byRoute[route]++
	m.recent = append(m.recent, reqRecord{
		Time:   time.Now().Format("15:04:05"),
		Method: method,
		Path:   path,
		Status: status,
		Ms:     ms,
	})
	if len(m.recent) > historySize {
		m.recent = m.recent[len(m.recent)-historySize:]
	}
}

func (m *metrics) snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	avg := int64(0)
	if m.total > 0 {
		avg = m.totalMs / m.total
	}
	routes := make(map[string]int64, len(m.byRoute))
	for k, v := range m.byRoute {
		routes[k] = v
	}
	// Newest first for the history rail.
	recent := make([]reqRecord, len(m.recent))
	for i, r := range m.recent {
		recent[len(m.recent)-1-i] = r
	}
	return map[string]any{
		"uptimeSec": int64(time.Since(m.start).Seconds()),
		"total":     m.total,
		"errors":    m.errors,
		"avgMs":     avg,
		"byRoute":   routes,
		"recent":    recent,
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// instrument logs and records every API request.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		ms := time.Since(start).Milliseconds()
		s.metrics.record(r.Method, r.URL.RequestURI(), rec.status, ms)
		log.Printf("%s %s -> %d (%dms)", r.Method, r.URL.RequestURI(), rec.status, ms)
	})
}
