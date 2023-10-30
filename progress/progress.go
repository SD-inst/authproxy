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
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/events"
)

//go:embed webroot
var webroot embed.FS

const maxTaskDuration = time.Minute * 5

type ProgressUpdate struct {
	Queued       int       `json:"queued"`
	Current      int       `json:"current"`
	Progress     float64   `json:"progress"`
	ETA          int       `json:"eta"`
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
		JobTimestamp  string `json:"job_timestamp"`
		Job           string `json:"job"`
		JobCount      int    `json:"job_count"`
		SamplingSteps int    `json:"sampling_steps"`
		SamplingStep  int    `json:"sampling_step"`
	}
}

type progress struct {
	b        *events.Broker
	sdhost   string
	fifoPath string
}

func NewProgress(broker *events.Broker, sdhost string, fifoPath string) *progress {
	return &progress{b: broker, sdhost: sdhost, fifoPath: fifoPath}
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
		if lastID != sdp.State.Job {
			lastID = sdp.State.Job
			jobStart, _ = time.ParseInLocation("20060102150405", sdp.State.JobTimestamp, time.Local)
			if time.Since(jobStart) > time.Hour { // sanity check
				jobStart = time.Now()
			}
		}
		if lastProgress != sdp.Progress {
			p.b.Broadcast(events.Packet{
				Type: events.PROGRESS_UPDATE,
				Data: ProgressUpdate{
					Current:      sdp.State.JobCount,
					Queued:       sdp.QueueSize,
					Progress:     sdp.Progress,
					ETA:          int(sdp.EtaRelative),
					Description:  fmt.Sprintf("%s %d/%d steps", "rendering", sdp.State.SamplingStep, sdp.State.SamplingSteps),
					LastActive:   time.Now(),
					TaskDuration: time.Since(jobStart).Truncate(time.Second).String(),
				}})
			lastProgress = sdp.Progress
		}
		if p.fifoPath != "" && time.Since(jobStart) > maxTaskDuration && sdp.Progress > 0 {
			log.Printf("Task execution time exceeded %s, restarting", maxTaskDuration.String())
			go func() {
				f, err := os.OpenFile(p.fifoPath, os.O_WRONLY, 0666)
				if err != nil {
					log.Printf("Error opening control FIFO: %s", err)
					return
				}
				f.WriteString("restart")
				f.Close()
				log.Printf("Instance restarted")
			}()
		}
	}
}

func (p *progress) gpuStatus() {
	cmd := exec.Command("nvidia-smi", "--query-gpu", "memory.used,memory.free,memory.total", "--format", "csv,noheader,nounits", "-l")
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
		used, _ := strconv.ParseUint(split[0], 10, 64)
		free, _ := strconv.ParseUint(split[1], 10, 64)
		total, _ := strconv.ParseUint(split[2], 10, 64)
		p.b.Broadcast(events.Packet{Type: events.GPU_UPDATE, Data: GPUUpdate{Free: free, Used: used, Total: total}})
	}
	cmd.Wait()
}

func (p *progress) Start() {
	go p.b.Start(context.Background())
	go p.updater()
	go p.gpuStatus()
}

func (p *progress) AddHandlers(e *echo.Group) {
	root, err := fs.Sub(webroot, "webroot")
	if err != nil {
		log.Fatal(err)
	}
	e.GET("/*", echo.StaticDirectoryHandler(root, false))
	e.GET("/ws", p.b.WSHandler)
}
