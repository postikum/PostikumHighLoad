package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

type Metric struct {
	Timestamp int64   `json:"timestamp"`
	CPU       float64 `json:"cpu"`
	RPS       float64 `json:"rps"`
}

type ringBuffer struct {
	mu    sync.Mutex
	data  []float64
	size  int
	idx   int
	count int
	sum   float64
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{data: make([]float64, size), size: size}
}

func (r *ringBuffer) add(v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == r.size {
		old := r.data[r.idx]
		r.sum -= old
	} else {
		r.count++
	}
	r.data[r.idx] = v
	r.sum += v
	r.idx = (r.idx + 1) % r.size
}

func (r *ringBuffer) snapshot() (vals []float64, avg float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		return []float64{}, 0
	}
	avg = r.sum / float64(r.count)

	vals = make([]float64, 0, r.count)
	start := 0
	if r.count == r.size {
		start = r.idx
	}
	for i := 0; i < r.count; i++ {
		pos := (start + i) % r.size
		vals = append(vals, r.data[pos])
	}
	return vals, avg
}

func meanStd(vals []float64) (mean, std float64) {
	n := float64(len(vals))
	if n == 0 {
		return 0, 0
	}
	var s float64
	for _, v := range vals {
		s += v
	}
	mean = s / n

	var varSum float64
	for _, v := range vals {
		d := v - mean
		varSum += d * d
	}
	std = math.Sqrt(varSum / n)
	return mean, std
}

type analyzer struct {
	window       *ringBuffer
	lastAvgRPS   atomic.Value
	lastZScore   atomic.Value
	lastAnomaly  atomic.Value
	anomalyTotal uint64
}

func newAnalyzer(windowSize int) *analyzer {
	a := &analyzer{window: newRingBuffer(windowSize)}
	a.lastAvgRPS.Store(float64(0))
	a.lastZScore.Store(float64(0))
	a.lastAnomaly.Store(false)
	return a
}

func (a *analyzer) process(m Metric) (avg, z float64, anomaly bool) {
	a.window.add(m.RPS)
	vals, avg := a.window.snapshot()
	mean, std := meanStd(vals)

	z = 0
	if std > 0 {
		z = (m.RPS - mean) / std
	}
	anomaly = math.Abs(z) > 2.0

	a.lastAvgRPS.Store(avg)
	a.lastZScore.Store(z)
	a.lastAnomaly.Store(anomaly)
	if anomaly {
		atomic.AddUint64(&a.anomalyTotal, 1)
	}
	return avg, z, anomaly
}

func (a *analyzer) stats() (avg, z float64, anomaly bool, total uint64) {
	avg = a.lastAvgRPS.Load().(float64)
	z = a.lastZScore.Load().(float64)
	anomaly = a.lastAnomaly.Load().(bool)
	total = atomic.LoadUint64(&a.anomalyTotal)
	return
}

var (
	reqTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "postikum_requests_total", Help: "Total HTTP requests"},
		[]string{"path", "code"},
	)
	reqLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "postikum_request_latency_seconds",
			Help:    "HTTP request latency",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"path"},
	)
	anomaliesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "postikum_anomalies_total", Help: "Total anomalies (|z|>2)"},
	)
	lastAvgGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "postikum_last_rolling_avg_rps", Help: "Last rolling avg RPS"},
	)
	lastZGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "postikum_last_zscore", Help: "Last z-score"},
	)
)

type respRecorder struct {
	http.ResponseWriter
	code int
}

func (r *respRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func withMetrics(path string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rr := &respRecorder{ResponseWriter: w, code: 200}
		h(rr, r)
		reqTotal.WithLabelValues(path, strconv.Itoa(rr.code)).Inc()
		reqLatency.WithLabelValues(path).Observe(time.Since(start).Seconds())
	}
}

func cacheMetric(ctx context.Context, rdb *redis.Client, m Metric) error {
	if rdb == nil {
		return nil
	}
	key := "postikum:rps:last"
	pipe := rdb.Pipeline()
	pipe.LPush(ctx, key, fmt.Sprintf("%.2f", m.RPS))
	pipe.LTrim(ctx, key, 0, 199)
	pipe.Incr(ctx, "postikum:ingest:count")
	pipe.Set(ctx, "postikum:last:timestamp", m.Timestamp, 0)
	_, err := pipe.Exec(ctx)
	return err
}

func getenv(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

func getenvInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func main() {
	port := getenv("PORT", "8080")
	redisAddr := getenv("REDIS_ADDR", "localhost:6379")
	redisPass := getenv("REDIS_PASSWORD", "")
	windowSize := getenvInt("WINDOW_SIZE", 50)

	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPass,
		DB:       0,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("WARN: redis ping failed: %v (service will still run)", err)
	}

	prometheus.MustRegister(reqTotal, reqLatency, anomaliesTotal, lastAvgGauge, lastZGauge)

	a := newAnalyzer(windowSize)
	metricsCh := make(chan Metric, 4096)

	go func() {
		for m := range metricsCh {
			avg, z, anomaly := a.process(m)
			lastAvgGauge.Set(avg)
			lastZGauge.Set(z)
			if anomaly {
				anomaliesTotal.Inc()
			}
			_ = avg
			_ = z
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("/ingest", withMetrics("/ingest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var m Metric
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if m.Timestamp == 0 {
			m.Timestamp = time.Now().UnixMilli()
		}

		select {
		case metricsCh <- m:
		default:
		}
		_ = cacheMetric(ctx, rdb, m)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "timestamp": m.Timestamp})
	}))

	mux.HandleFunc("/analyze", withMetrics("/analyze", func(w http.ResponseWriter, r *http.Request) {
		avg, z, anomaly, total := a.stats()

		redisLen := int64(-1)
		if n, err := rdb.LLen(ctx, "postikum:rps:last").Result(); err == nil {
			redisLen = n
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"window":          windowSize,
			"rolling_avg_rps": avg,
			"last_zscore":     z,
			"last_anomaly":    anomaly,
			"anomalies_total": total,
			"redis_list_len":  redisLen,
		})
	}))

	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}

	log.Printf("PostikumHighload listening on :%s (redis=%s, window=%d)", port, redisAddr, windowSize)
	log.Fatal(srv.ListenAndServe())
}
