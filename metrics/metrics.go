package metrics

import (
	"fmt"
	"log"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	reg          prometheus.Registerer
	metrics      map[MetricID]prometheus.Metric
	updater      chan MetricUpdate
	pushPassword string
}

type MetricID int

const (
	TASKS_COMPLETED MetricID = iota
	GPU_ACTIVE_TIME
	QUEUE_LENGTH
	GPU_FREE_MEMORY
	GPU_USED_MEMORY
	GPU_JOULES_SPENT
	UPLOAD_COUNT
	UPLOAD_SIZE
	LLM_TOKENS
)

type MetricUpdate struct {
	Type  MetricID
	Value float64
}

func (m *Metrics) start() {
	for u := range m.updater {
		if metric, ok := m.metrics[u.Type]; ok {
			switch t := metric.(type) {
			case prometheus.Gauge:
				t.Set(u.Value)
			case prometheus.Counter:
				t.Add(u.Value)
			}
		} else {
			log.Printf("Unknown metric of type %v", u.Type)
		}
	}
}

func (m *Metrics) register(id MetricID, t prometheus.ValueType, name string, help string) {
	var newMetric prometheus.Metric
	switch t {
	case prometheus.CounterValue:
		newMetric = prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	case prometheus.GaugeValue:
		newMetric = prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	default:
		panic(fmt.Sprintf("Unknown metric value type: %d", t))
	}
	m.metrics[id] = newMetric
	m.reg.MustRegister(newMetric.(prometheus.Collector))
}

func (m *Metrics) handleMetricPush(c echo.Context) error {
	var params struct {
		Password string  `json:"password"`
		Name     string  `json:"name"`
		Value    float64 `json:"value"`
	}
	if err := c.Bind(&params); err != nil {
		log.Printf("Error binding params: %s", err)
		return err
	}
	if params.Password != m.pushPassword {
		return c.String(401, "Invalid password")
	}
	switch params.Name {
	case "llm":
		m.updater <- MetricUpdate{Type: LLM_TOKENS, Value: params.Value}
	default:
		return c.String(400, "Invalid name")
	}
	return nil
}

func NewMetrics(e *echo.Echo, pushPassword string) chan<- MetricUpdate {
	m := Metrics{reg: prometheus.NewRegistry(), metrics: map[MetricID]prometheus.Metric{}, updater: make(chan MetricUpdate, 100), pushPassword: pushPassword}
	m.register(TASKS_COMPLETED, prometheus.CounterValue, "tasks_completed", "Number of tasks processed")
	m.register(GPU_ACTIVE_TIME, prometheus.CounterValue, "gpu_active_time", "Number of seconds the GPU was spinning")
	m.register(QUEUE_LENGTH, prometheus.GaugeValue, "queue_length", "Number of tasks queued for processing")
	m.register(GPU_FREE_MEMORY, prometheus.GaugeValue, "gpu_free_memory", "Amount of free VRAM")
	m.register(GPU_USED_MEMORY, prometheus.GaugeValue, "gpu_used_memory", "Amount of occupied VRAM")
	m.register(GPU_JOULES_SPENT, prometheus.CounterValue, "gpu_joules_spent", "Amount of joules converted to warm the air")
	m.register(UPLOAD_COUNT, prometheus.CounterValue, "upload_count", "Number of LoRAs uploaded")
	m.register(UPLOAD_SIZE, prometheus.CounterValue, "upload_size", "Total size of LoRAs uploaded")
	m.register(LLM_TOKENS, prometheus.CounterValue, "llm_tokens", "Total number of tokens generated with the LLM")

	h := promhttp.HandlerFor(m.reg.(prometheus.Gatherer), promhttp.HandlerOpts{Registry: m.reg})
	e.GET("/metrics", func(c echo.Context) error {
		h.ServeHTTP(c.Response(), c.Request())
		return nil
	})
	if m.pushPassword != "" {
		e.POST("/metrics", m.handleMetricPush)
	}
	go m.start()
	return m.updater
}
