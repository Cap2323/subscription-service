package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type Metrics struct {
	Registry   *prometheus.Registry
	Requests   *prometheus.CounterVec
	Duration   *prometheus.HistogramVec
	Inflight   prometheus.Gauge
	Pool       *prometheus.GaugeVec
	ShardReady *prometheus.GaugeVec
}

func New() *Metrics {
	m := &Metrics{Registry: prometheus.NewRegistry(),
		Requests:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "subscriptions_http_requests_total", Help: "Completed HTTP requests."}, []string{"route", "method", "status"}),
		Duration:   prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "subscriptions_http_duration_seconds", Help: "HTTP latency.", Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}}, []string{"route", "method"}),
		Inflight:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "subscriptions_http_inflight", Help: "Active HTTP requests."}),
		ShardReady: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "subscriptions_shard_ready", Help: "One when a shard accepts the current topology."}, []string{"shard"}),
		Pool:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "subscriptions_db_connections", Help: "Connections per shard and state."}, []string{"shard", "state"})}
	m.Registry.MustRegister(m.Requests, m.Duration, m.Inflight, m.Pool, m.ShardReady, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}
