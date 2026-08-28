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

type sdprogressState struct {
	JobTimestamp  string  `json:"job_timestamp"`
	Job           *string `json:"job"`
	Node          string  `json:"node"`
	JobCount      int     `json:"job_count"`
	SamplingSteps int     `json:"sampling_steps"`
	SamplingStep  int     `json:"sampling_step"`
}

type sdprogress struct {
	Progress    float64 `json:"progress"`
	EtaRelative float64 `json:"eta_relative"`
	QueueSize   int     `json:"queue_size"`
	State       sdprogressState
}

// CUIReseter lets callers (the /cui/join handler) reset the CUI progress
// state at task start without depending on the unexported progress type.
type CUIReseter interface {
	ResetCUI()
}

type progress struct {
	b           *events.Broker
	sdhost      string
	wd          *watchdog.Watchdog
	timeout     time.Duration
	m           chan<- metrics.MetricUpdate
	svcChan     <-chan servicequeue.SvcUpdate
	pchan       chan sdprogress
	statusToken string
	sq          *servicequeue.ServiceQueue
	resetCUI    chan struct{}
}

func NewProgress(broker *events.Broker, sdhost string, timeout int, wd *watchdog.Watchdog, m chan<- metrics.MetricUpdate, svcChan <-chan servicequeue.SvcUpdate, statusToken string, sq *servicequeue.ServiceQueue) *progress {
	return &progress{b: broker, sdhost: sdhost, timeout: time.Second * time.Duration(timeout), wd: wd, m: m, svcChan: svcChan, pchan: make(chan sdprogress, 100), statusToken: statusToken, sq: sq, resetCUI: make(chan struct{}, 1)}
}

// ResetCUI resets the CUI progress state and all ETA timers at task start
// (ComfyUI does not send a zero-progress on task start, so the previous
// task's 100% would otherwise linger until the next sampler's first step).
// Non-blocking: if the updater is already resetting, the signal is dropped.
func (p *progress) ResetCUI() {
	select {
	case p.resetCUI <- struct{}{}:
	default:
	}
}

func (p *progress) updater() {
	lastProgress := float64(0)
	lastID := ""
	jobStart := time.Time{}
	nodeStart := time.Time{}
	adjustedNodeStart := time.Time{}
	lastNode := ""
	lastCUI := false
	nodeRefStart := time.Time{}      // last progress of the previous node (gap reference)
	prevNodeRate := time.Duration(0) // per-step rate of the previous node (first-step prior)
	nodeStartValue := 0
	lastMax := 0
	lastStep := 0
	lastStepTs := time.Time{}
	stepObs := 0
	lastProgTs := time.Time{}
	lastJobCount := 0
	lastQueue := 0
	lastProg := float64(0)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	// Remaining ETA, computed over the same span the progress fraction covers.
	//
	// CUI (ComfyUI): progress is per-node (value/max resets each node), and
	// nodeStart is the FIRST progress of the node — which arrives after step
	// nodeStartValue (usually 1). So the elapsed span covers (lastStep -
	// nodeStartValue) steps, not lastStep; the rate is measured over that span
	// and remaining is (lastMax - lastStep) steps at that rate. This keeps the
	// ETA stable from the second step on (like CozyUI).
	//
	// A1111 (SDF): no node id, progress is the job-wide fraction, so the
	// baseline is jobStart and the rate is over the whole job (batch-wide
	// progress; the per-task sampling_step would make the ETA jump). The first
	// increment (often model loading) is re-timed via adjustedNodeStart.
	//
	// The estimate is aged down each second until the next update. ok=false
	// when there is no baseline yet.
	computeEta := func(now time.Time) (time.Duration, bool) {
		if lastProgTs.IsZero() {
			return 0, false
		}
		var elapsed time.Duration
		var remaining time.Duration
		if lastCUI {
			units := lastStep - nodeStartValue
			if units < 0 {
				return 0, false
			}
			if units == 0 {
				// Only the first step is known and there is no measured span
				// yet. Estimate the first step's duration: prefer the previous
				// node's per-step rate (a typical step time, excludes setup);
				// fall back to the gap to the previous event capped at 60s
				// (the gap includes setup time, so it overestimates).
				// Corrects on the second step.
				var stepEst time.Duration
				if prevNodeRate > 0 {
					stepEst = prevNodeRate
				} else if !nodeRefStart.IsZero() {
					stepEst = min(nodeStart.Sub(nodeRefStart), 60*time.Second)
				}
				if stepEst <= 0 {
					return 0, false
				}
				remaining = time.Duration(float64(stepEst) * float64(lastMax-lastStep))
			} else {
				elapsed = lastProgTs.Sub(nodeStart)
				if elapsed <= 0 {
					return 0, false
				}
				remaining = time.Duration(float64(elapsed) / float64(units) * float64(lastMax-lastStep))
			}
		} else {
			if lastProg <= 0 || lastProg >= 1 {
				return 0, false
			}
			start := jobStart
			if !adjustedNodeStart.IsZero() {
				start = adjustedNodeStart
			}
			elapsed = lastProgTs.Sub(start)
			if elapsed <= 0 {
				return 0, false
			}
			remaining = time.Duration(float64(elapsed) * (1 - lastProg) / lastProg)
		}
		age := now.Sub(lastProgTs)
		if age < 0 {
			age = 0
		}
		eta := remaining - age
		if eta < 0 {
			eta = 0
		}
		return eta, true
	}

	broadcast := func(desc string) {
		now := time.Now()
		etaStr := ""
		if eta, ok := computeEta(now); ok {
			etaStr = eta.Truncate(time.Second).String()
		}
		p.b.Broadcast(events.Packet{
			Type: events.PROGRESS_UPDATE,
			Data: ProgressUpdate{
				Current:      lastJobCount,
				Queued:       lastQueue,
				Progress:     lastProg,
				ETA:          etaStr,
				Description:  desc,
				LastActive:   now,
				TaskDuration: now.Sub(jobStart).Truncate(time.Second).String(),
			}})
	}

	for {
		select {
		case <-p.resetCUI:
			// Task start: ComfyUI does not send zero-progress here, so clear
			// the previous task's state and all ETA baselines (the progress
			// would otherwise linger at 100% until the next sampler step).
			lastProg = 0
			lastProgTs = time.Time{}
			jobStart = time.Now()
			lastID = ""
			nodeStart = time.Time{}
			adjustedNodeStart = time.Time{}
			lastNode = ""
			lastCUI = false
			nodeRefStart = time.Time{}
			prevNodeRate = 0
			nodeStartValue = 0
			lastMax = 0
			lastStep = 0
			lastStepTs = time.Time{}
			stepObs = 0
			broadcast("")
		case sdp := <-p.pchan:
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
			// Node boundary: ComfyUI sends a per-node id (value/max resets each
			// node), so the ETA baseline is re-timed on every node. A1111 has
			// no node id, so the job name stays the boundary (SDF behaviour is
			// unchanged).
			nodeKey := *sdp.State.Job
			if sdp.State.Node != "" {
				nodeKey = sdp.State.Node
			}
			if lastNode != nodeKey {
				if nodeKey != "" {
					// Capture the previous node's last progress time (gap
					// reference) and per-step rate (first-step prior).
					nodeRefStart = lastProgTs
					if pu := lastStep - nodeStartValue; pu > 0 && !nodeStart.IsZero() && !lastProgTs.IsZero() {
						prevNodeRate = lastProgTs.Sub(nodeStart) / time.Duration(pu)
					} else {
						prevNodeRate = 0
					}
					nodeStart = time.Now()
					nodeStartValue = sdp.State.SamplingStep // first progress of the node (usually step 1)
					adjustedNodeStart = time.Time{}
					lastStep = 0
					lastStepTs = time.Time{}
					stepObs = 0
					lastProgTs = time.Time{}
					lastProg = -1
				}
				lastNode = nodeKey
			}
			lastCUI = sdp.State.Node != ""
			lastMax = sdp.State.SamplingSteps
			lastJobCount = sdp.State.JobCount
			lastQueue = sdp.QueueSize
			if sdp.Progress != lastProg {
				lastProgTs = time.Now()
			}
			lastProg = sdp.Progress
			if sdp.State.SamplingStep != lastStep {
				now := time.Now()
				if sdp.State.SamplingStep > lastStep {
					stepObs++
					// The first observed increment often includes model
					// loading. On the second observation, re-time it to the
					// second's per-step rate when that is significantly
					// faster (10% threshold), regardless of how many steps
					// each poll jumped.
					if stepObs == 2 {
						firstDur := lastStepTs.Sub(nodeStart)
						secondDur := now.Sub(lastStepTs)
						if firstDur > 0 && secondDur > 0 {
							firstRate := firstDur / time.Duration(lastStep)
							secondRate := secondDur / time.Duration(sdp.State.SamplingStep-lastStep)
							if secondRate > 0 && 10*secondRate <= 9*firstRate {
								adjustedNodeStart = nodeStart.Add(firstDur - time.Duration(lastStep)*secondRate)
							}
						}
					}
				}
				lastStep = sdp.State.SamplingStep
				lastStepTs = now
			}
			if lastProgress != sdp.Progress {
				desc := fmt.Sprintf("%s %d/%d steps", "rendering", sdp.State.SamplingStep, sdp.State.SamplingSteps)
				updateDesc := p.sq.UpdateProgressDescription(desc)
				descForBroadcast := desc
				if !updateDesc {
					descForBroadcast = ""
				}
				broadcast(descForBroadcast)
				lastProgress = sdp.Progress
				p.m <- metrics.MetricUpdate{Type: metrics.QUEUE_LENGTH, Value: float64(sdp.QueueSize)}
			}
			if p.wd != nil && time.Since(jobStart) > p.timeout && sdp.Progress > 0 {
				log.Printf("Task execution time exceeded %s, restarting", p.timeout.String())
				p.wd.Send("restart stablediff-cuda")
			}
		case <-ticker.C:
			// Tick the ETA/duration down each second while a job is running.
			if lastID != "" {
				broadcast("")
			}
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
	if err := s.Err(); err != nil {
		log.Printf("Error scanning nvidia-smi output: %s", err)
	}
	cmd.Wait()
}

func (p *progress) serviceUpdater() {
	var lastDescription string
	var lastPrevService servicequeue.SvcType
	var lastPrevWaitService servicequeue.SvcType
	for svc := range p.svcChan {
		resp := p.b.State(events.SERVICE_UPDATE)
		event := events.ServiceUpdate{Service: svc.Type, WaitService: svc.WaitType, LastActive: time.Now(), Queue: svc.Queue}
		if svc.Description != "" {
			event.Description = svc.Description
			lastDescription = svc.Description
		} else if pkt, ok := resp.(events.Packet); ok && pkt.Type == events.SERVICE_UPDATE {
			if prevSvc, ok := pkt.Data.(events.ServiceUpdate); ok {
				event.Description = prevSvc.Description
			}
		} else {
			event.Description = lastDescription
		}
		if pkt, ok := resp.(events.Packet); ok && pkt.Type == events.SERVICE_UPDATE {
			prevSvc := pkt.Data.(events.ServiceUpdate)
			if svc.Type == servicequeue.IGNORE {
				event.Service = prevSvc.Service
				svc.Type = prevSvc.Service
			}
			if prevSvc.Service != svc.Type {
				event.PrevService = prevSvc.Service
				event.PrevWaitService = prevSvc.WaitService
			} else {
				event.PrevService = lastPrevService
				event.PrevWaitService = lastPrevWaitService
			}
		}
		lastPrevService = event.PrevService
		lastPrevWaitService = event.PrevWaitService
		p.b.Broadcast(events.Packet{Type: events.SERVICE_UPDATE, Data: event})
	}
}

func (p *progress) sdQuery(sq *servicequeue.ServiceQueue) {
	client := http.Client{Timeout: time.Second * 5}
	for {
		time.Sleep(time.Second)
		sq.Lock()
		sq.AwaitCheck(servicequeue.SD, true, false, nil)
		sq.Unlock()
		resp, err := client.Get(p.sdhost + "/sdapi/v1/progress")
		if err != nil {
			log.Printf("Error getting data: %v", err)
			continue
		}
		var sdp sdprogress
		json.NewDecoder(resp.Body).Decode(&sdp)
		p.pchan <- sdp
	}
}

func (p *progress) handleCUIProgress(c echo.Context) error {
	var params struct {
		Value float64 `json:"value"`
		Max   float64 `json:"max"`
		Queue int     `json:"queue"`
		Job   string  `json:"prompt_id"`
		Node  string  `json:"node"`
	}
	c.Bind(&params)
	p.pchan <- sdprogress{Progress: params.Value / params.Max, QueueSize: params.Queue - 1, State: sdprogressState{Job: &params.Job, Node: params.Node, SamplingSteps: int(params.Max), SamplingStep: int(params.Value), JobCount: 1}}
	p.sq.SetService(servicequeue.CUI, fmt.Sprintf("rendering %d/%d steps", int(params.Value), int(params.Max)))
	return nil
}

type statusJSON struct {
	Progress        float64 `json:"progress"`
	TaskQueue       int     `json:"task_queue"`
	ServiceQueue    int32   `json:"service_queue"`
	Service         string  `json:"service"`
	WaitService     string  `json:"wait_service"`
	PrevService     string  `json:"prev_service"`
	PrevWaitService string  `json:"prev_wait_service"`
	ETA             string  `json:"eta"`
}

func (p *progress) handleStatusJSON(c echo.Context) error {
	if p.statusToken != "" {
		token := c.QueryParam("token")
		if token != p.statusToken {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		}
	}

	var progProgress float64
	var progQueued int
	var progETA string

	progResp := p.b.State(events.PROGRESS_UPDATE)
	if progResp != nil {
		if pkt, ok := progResp.(events.Packet); ok {
			if pu, ok := pkt.Data.(ProgressUpdate); ok {
				progProgress = pu.Progress
				progQueued = pu.Queued
				progETA = pu.ETA
			}
		}
	}

	var svcQueue int32
	var svcName string
	var waitSvcName string
	var prevSvcName string
	var prevWaitSvcName string
	var su events.ServiceUpdate

	svcResp := p.b.State(events.SERVICE_UPDATE)
	if svcResp != nil {
		if pkt, ok := svcResp.(events.Packet); ok {
			if suData, ok := pkt.Data.(events.ServiceUpdate); ok {
				su = suData
				svcQueue = su.Queue
				svcName = su.Service.String()
				waitSvcName = su.WaitService.String()
				prevSvcName = su.PrevService.String()
				prevWaitSvcName = su.PrevWaitService.String()
			}
		}
	}

	return c.JSON(http.StatusOK, statusJSON{
		Progress:        progProgress,
		TaskQueue:       progQueued,
		ServiceQueue:    svcQueue,
		Service:         svcName,
		WaitService:     waitSvcName,
		PrevService:     prevSvcName,
		PrevWaitService: prevWaitSvcName,
		ETA:             progETA,
	})
}

func (p *progress) Start(sq *servicequeue.ServiceQueue) {
	go p.b.Start(context.Background())
	go p.updater()
	go p.sdQuery(sq)
	go p.gpuStatus()
	go p.serviceUpdater()
}

func (p *progress) AddHandlers(e *echo.Echo) {
	root, err := fs.Sub(webroot, "webroot")
	if err != nil {
		log.Fatal(err)
	}
	e.GET("/q/*", echo.StaticDirectoryHandler(root, false))
	e.GET("/q/ws", p.b.WSHandler)
	e.GET("/q/status.json", p.handleStatusJSON)
	e.POST("/cui/progress", p.handleCUIProgress)
}
