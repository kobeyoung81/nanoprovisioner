// Package metrics exposes the provisioner's operational numbers.
//
// The plan puts three questions here — how many tunnels are live, how long a
// forwarded request takes, and how big the bodies are — because a container is
// reached through this module and nowhere else. The HTTP and database numbers are
// the same three every module reports, so one dashboard works for all of them.
//
// The scrape endpoint is anonymous and lives on the internal HTTP port next to
// /healthz: a scraper is infrastructure, not a user, and the port is not public.
package metrics

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// HTTPRequests counts calls by matched route pattern, method and status.
	// The pattern comes from the ServeMux (Go 1.22+), so a path parameter never
	// becomes a label value and the series count stays bounded.
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "provisioner_http_requests_total",
		Help: "HTTP requests served, by route, method and status.",
	}, []string{"route", "method", "status"})

	// HTTPSeconds is the handler latency of those calls.
	HTTPSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "provisioner_http_request_seconds",
		Help:    "Time spent serving one HTTP request, by route.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"route"})

	// ActiveTunnels is the number of hypervisor connections this replica holds.
	// A drop to zero is the difference between "the platform is quiet" and "no
	// container can be reached".
	ActiveTunnels = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "provisioner_active_tunnels",
		Help: "Hypervisor tunnels registered on this replica.",
	})

	// TunnelRequests counts forwarded requests by outcome (ok, offline, busy,
	// timeout, failed, too_large).
	TunnelRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "provisioner_tunnel_requests_total",
		Help: "Tunnel requests forwarded, by outcome.",
	}, []string{"result"})

	// TunnelRequestSeconds is how long a container took to answer. The tail is
	// what matters: the forward timeout is a minute, and a p99 near it means the
	// container is unwell, not that the network is slow.
	TunnelRequestSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "provisioner_tunnel_request_seconds",
		Help:    "Time from forwarding a tunnel request to its answer.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	})

	// TunnelBodyBytes is the size of the body a caller asked to forward.
	TunnelBodyBytes = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "provisioner_tunnel_body_bytes",
		Help:    "Body size of forwarded tunnel requests, in bytes.",
		Buckets: []float64{1024, 4096, 16384, 65536, 262144, 1 << 20, 2 << 20},
	})
)

func init() {
	prometheus.MustRegister(HTTPRequests, HTTPSeconds, ActiveTunnels,
		TunnelRequests, TunnelRequestSeconds, TunnelBodyBytes)
}

// Handler serves the Prometheus text exposition.
func Handler() http.Handler { return promhttp.Handler() }

// Middleware records one HTTP call under its route pattern. A request that
// matched no route is reported as "unmatched" rather than by path: an unknown
// path is exactly the sort of thing that would otherwise mint a time series per
// probe.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
		HTTPSeconds.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

// statusRecorder keeps the status the handler wrote. A handler that never calls
// WriteHeader has answered 200, which is what the initial value records.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status, r.written = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// Flush keeps streaming handlers working through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack keeps WebSocket upgrades working through the wrapper: the gorilla
// upgrader takes the connection over, and a wrapper that hides net.Conn would
// break every tunnel.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
