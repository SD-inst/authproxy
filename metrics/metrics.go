package metrics

import (
	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	tasksCompleted prometheus.Counter
	gpuActiveTime  prometheus.Counter
	queueLength    prometheus.Gauge
	gpuFreeMemory  prometheus.Gauge
	gpuUsedMemory  prometheus.Gauge
	uploadCount    prometheus.Counter
	uploadSize     prometheus.Counter
	updater        chan MetricUpdate
}

type MetricType int

const (
	TASKS_COMPLETED MetricType = iota
	GPU_ACTIVE_TIME
	QUEUE_LENGTH
	GPU_FREE_MEMORY
	GPU_USED_MEMORY
	UPLOAD_COUNT
	UPLOAD_SIZE
)

type MetricUpdate struct {
	Type  MetricType
	Value float64
}

func (m *Metrics) start() {
	for u := range m.updater {
		switch u.Type {
		case TASKS_COMPLETED:
			m.tasksCompleted.Add(u.Value)
		case GPU_ACTIVE_TIME:
			m.gpuActiveTime.Add(u.Value)
		case QUEUE_LENGTH:
			m.queueLength.Set(u.Value)
		case GPU_FREE_MEMORY:
			m.gpuFreeMemory.Set(u.Value)
		case GPU_USED_MEMORY:
			m.gpuUsedMemory.Set(u.Value)
		case UPLOAD_COUNT:
			m.uploadCount.Add(u.Value)
		case UPLOAD_SIZE:
			m.uploadSize.Add(u.Value)
		}
	}
}

func NewMetrics(e *echo.Echo) chan<- MetricUpdate {
	reg := prometheus.NewRegistry()
	m := Metrics{
		tasksCompleted: prometheus.NewCounter(prometheus.CounterOpts{Name: "tasks_completed", Help: "Number of tasks processed"}),
		gpuActiveTime:  prometheus.NewCounter(prometheus.CounterOpts{Name: "gpu_active_time", Help: "Number of seconds the GPU was spinning"}),
		queueLength:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "queue_length", Help: "Number of tasks queued for processing"}),
		gpuFreeMemory:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "gpu_free_memory", Help: "Amount of free VRAM"}),
		gpuUsedMemory:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "gpu_used_memory", Help: "Amount of occupied VRAM"}),
		uploadCount:    prometheus.NewCounter(prometheus.CounterOpts{Name: "upload_count", Help: "Number of LoRAs uploaded"}),
		uploadSize:     prometheus.NewCounter(prometheus.CounterOpts{Name: "upload_size", Help: "Total size of LoRAs uploaded"}),
		updater:        make(chan MetricUpdate, 100)}
	reg.MustRegister(m.tasksCompleted)
	reg.MustRegister(m.gpuActiveTime)
	reg.MustRegister(m.queueLength)
	reg.MustRegister(m.gpuFreeMemory)
	reg.MustRegister(m.gpuUsedMemory)
	reg.MustRegister(m.uploadCount)
	reg.MustRegister(m.uploadSize)
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
	e.GET("/metrics", func(c echo.Context) error {
		h.ServeHTTP(c.Response(), c.Request())
		return nil
	})
	go m.start()
	return m.updater
}
