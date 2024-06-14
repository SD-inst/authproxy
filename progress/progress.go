package progress

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/events"
	"github.com/rkfg/authproxy/metrics"
	"github.com/rkfg/authproxy/servicequeue"
	"github.com/rkfg/authproxy/watchdog"
)

//go:embed webroot
var webroot embed.FS

type ProgressUpdate struct {
	Queued       int       `json:"queued"`
	Current      int       `json:"current"`
	Progress     float64   `json:"progress"`
	ETA          string    `json:"eta"`
	Description  string    `json:"description"`
	LastActive   time.Time `json:"last_active"`
	TaskDuration string    `json:"duration"`
}

type GPUUpdate struct {
	Used  uint64 `json:"used"`
	Free  uint64 `json:"free"`
	Total uint64 `json:"total"`
}

type sdprogress struct {
	Progress    float64 `json:"progress"`
	EtaRelative float64 `json:"eta_relative"`
	QueueSize   int     `json:"queue_size"`
	State       struct {
		JobTimestamp  string  `json:"job_timestamp"`
		Job           *string `json:"job"`
		JobCount      int     `json:"job_count"`
		SamplingSteps int     `json:"sampling_steps"`
		SamplingStep  int     `json:"sampling_step"`
	}
}

type progress struct {
	b       *events.Broker
	sdhost  string
	wd      *watchdog.Watchdog
	timeout time.Duration
	m       chan<- metrics.MetricUpdate
	svcChan <-chan servicequeue.SvcType
}

func NewProgress(broker *events.Broker, sdhost string, timeout int, wd *watchdog.Watchdog, m chan<- metrics.MetricUpdate, svcChan <-chan servicequeue.SvcType) *progress {
	return &progress{b: broker, sdhost: sdhost, timeout: time.Second * time.Duration(timeout), wd: wd, m: m, svcChan: svcChan}
}

func (p *progress) updater() {
	client := http.Client{Timeout: time.Second * 5}
	lastProgress := float64(0)
	lastID := ""
	jobStart := time.Now()
	for {
		time.Sleep(time.Second)
		resp, err := client.Get(p.sdhost + "/sdapi/v1/progress")
		if err != nil {
			log.Printf("Error getting data: %v", err)
			continue
		}
		var sdp sdprogress
		json.NewDecoder(resp.Body).Decode(&sdp)
		if sdp.State.Job == nil {
			continue
		}
		if lastID != *sdp.State.Job {
			if lastID != "" {
				p.m <- metrics.MetricUpdate{Type: metrics.GPU_ACTIVE_TIME, Value: time.Since(jobStart).Seconds()}
			}
			if *sdp.State.Job != "" {
				p.m <- metrics.MetricUpdate{Type: metrics.TASKS_COMPLETED, Value: 1} // actually not completed but started but most tasks eventually complete so whatever
				jobStart = time.Now()
			}
			lastID = *sdp.State.Job
		}
		if lastProgress != sdp.Progress {
			p.b.Broadcast(events.Packet{
				Type: events.PROGRESS_UPDATE,
				Data: ProgressUpdate{
					Current:      sdp.State.JobCount,
					Queued:       sdp.QueueSize,
					Progress:     sdp.Progress,
					ETA:          time.Duration(sdp.EtaRelative * float64(time.Second)).Truncate(time.Second).String(),
					Description:  fmt.Sprintf("%s %d/%d steps", "rendering", sdp.State.SamplingStep, sdp.State.SamplingSteps),
					LastActive:   time.Now(),
					TaskDuration: time.Since(jobStart).Truncate(time.Second).String(),
				}})
			lastProgress = sdp.Progress
			p.m <- metrics.MetricUpdate{Type: metrics.QUEUE_LENGTH, Value: float64(sdp.QueueSize)}
		}
		if p.wd != nil && time.Since(jobStart) > p.timeout && sdp.Progress > 0 {
			log.Printf("Task execution time exceeded %s, restarting", p.timeout.String())
			p.wd.Send("restart stablediff-cuda")
		}
	}
}

func (p *progress) gpuStatus() {
	cmd := exec.Command("nvidia-smi", "--query-gpu", "memory.used,memory.free,memory.total,power.draw", "--format", "csv,noheader,nounits", "-l", "1")
	output, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Error getting stdout of nvidia-smi: %s", err)
		return
	}
	s := bufio.NewScanner(output)
	if err := cmd.Start(); err != nil {
		log.Printf("Error starting nvidia-smi: %s", err)
		return
	}
	for s.Scan() {
		line := s.Text()
		split := strings.Split(line, ", ")
		if len(split) < 4 {
			log.Printf("GPU monitoring error, read line: %s", line)
			return
		}
		used, _ := strconv.ParseUint(split[0], 10, 64)
		free, _ := strconv.ParseUint(split[1], 10, 64)
		total, _ := strconv.ParseUint(split[2], 10, 64)
		watts, _ := strconv.ParseFloat(split[3], 64)
		p.b.Broadcast(events.Packet{Type: events.GPU_UPDATE, Data: GPUUpdate{Free: free, Used: used, Total: total}})
		p.m <- metrics.MetricUpdate{Type: metrics.GPU_FREE_MEMORY, Value: float64(free)}
		p.m <- metrics.MetricUpdate{Type: metrics.GPU_USED_MEMORY, Value: float64(used)}
		p.m <- metrics.MetricUpdate{Type: metrics.GPU_JOULES_SPENT, Value: float64(watts)}
	}
	cmd.Wait()
}

func (p *progress) serviceUpdater() {
	for svc := range p.svcChan {
		resp := p.b.State(events.SERVICE_UPDATE)
		desc := ""
		if p, ok := resp.(events.Packet); ok && p.Type == events.SERVICE_UPDATE {
			prevSvc := p.Data.(events.ServiceUpdate)
			if prevSvc.Service != svc {
				desc = fmt.Sprintf("Service switch from %s to %s", prevSvc.Service, svc)
			}
		}
		p.b.Broadcast(events.Packet{Type: events.SERVICE_UPDATE, Data: events.ServiceUpdate{
			Service: svc,
		}})
		p.b.Broadcast(events.Packet{Type: events.PROGRESS_UPDATE, Data: ProgressUpdate{LastActive: time.Now(), Description: desc}})
	}
}

func (p *progress) Start() {
	go p.b.Start(context.Background())
	go p.updater()
	go p.gpuStatus()
	go p.serviceUpdater()
}

func (p *progress) AddHandlers(e *echo.Group) {
	root, err := fs.Sub(webroot, "webroot")
	if err != nil {
		log.Fatal(err)
	}
	e.GET("/*", echo.StaticDirectoryHandler(root, false))
	e.GET("/ws", p.b.WSHandler)
}
