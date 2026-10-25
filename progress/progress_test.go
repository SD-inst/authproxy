package progress

import (
	"context"
	"testing"
	"time"

	"github.com/rkfg/authproxy/events"
	"github.com/rkfg/authproxy/metrics"
	"github.com/rkfg/authproxy/servicequeue"
)

// startUpdater wires only the parts a test drives — the updater and the service
// updater on a real broker — and returns the channel to feed service
// transitions through, the broker, and the controls the balancers use. No SD
// polling, no GPU monitoring, no watchdog.
func startUpdater(t *testing.T) (svcChan chan servicequeue.SvcUpdate, broker *events.Broker, controls QueueControls) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	broker = events.NewBroker()
	go broker.Start(ctx)
	svcChan = make(chan servicequeue.SvcUpdate, 100)
	m := make(chan metrics.MetricUpdate, 100)
	p := NewProgress(broker, "http://127.0.0.1:1", 300, nil, m, svcChan, "", nil)
	go p.updater()
	go p.serviceUpdater()
	t.Cleanup(cancel)
	return svcChan, broker, p
}

// waitForProgress polls the broker's cached last progress packet until pred
// accepts it, failing the test when that never happens.
func waitForProgress(t *testing.T, b *events.Broker, pred func(ProgressUpdate) bool) ProgressUpdate {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if pkt, ok := b.State(events.PROGRESS_UPDATE).(events.Packet); ok {
			if pu, ok := pkt.Data.(ProgressUpdate); ok && pred(pu) {
				return pu
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the expected progress packet")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An LLM request holds the service slot but never reports a progress fraction.
// The page must still see one active task, an indeterminate (full, striped)
// bar, the key's color, and the generated-token count as the task line.
func TestLLMTaskVisibleWithoutProgressReports(t *testing.T) {
	svcChan, broker, controls := startUpdater(t)

	svcChan <- servicequeue.SvcUpdate{Type: servicequeue.LLM, Description: "test-model"}
	pu := waitForProgress(t, broker, func(pu ProgressUpdate) bool { return pu.Indeterminate })
	if pu.Current != 1 {
		t.Errorf("expected current = 1 while the LLM holds the slot, got %d", pu.Current)
	}
	if pu.Progress != 1 {
		t.Errorf("expected an indeterminate task to report a full bar, got %v", pu.Progress)
	}

	controls.SetTaskColor("#22c55e")
	controls.AddTokens("magnum-test", 40)
	controls.AddTokens("magnum-test", 60)
	// The counter accumulates and keeps the model, so the last packet carries
	// the whole total next to the model that generated it.
	pu = waitForProgress(t, broker, func(pu ProgressUpdate) bool { return pu.Description == "magnum-test: 100 tokens generated" })
	if pu.Color != "#22c55e" {
		t.Errorf("expected the configured key color in the packet, got %q", pu.Color)
	}

	// The slot is freed: no active task, no indeterminate bar, and the token
	// line is cleared with the task.
	svcChan <- servicequeue.SvcUpdate{Type: servicequeue.WAIT, WaitType: servicequeue.LLM}
	pu = waitForProgress(t, broker, func(pu ProgressUpdate) bool {
		return !pu.Indeterminate && pu.Current == 0 && pu.Description == ""
	})
	if pu.Color != "" {
		t.Errorf("expected the color to clear with the slot, got %q", pu.Color)
	}
}

// The token line is the task line, so the per-second ETA tick must not wipe
// it: without that, the count would flash for a second and fall back to the
// model name while the task is still running.
func TestTokenLineSurvivesTicks(t *testing.T) {
	svcChan, broker, controls := startUpdater(t)

	svcChan <- servicequeue.SvcUpdate{Type: servicequeue.LLM}
	waitForProgress(t, broker, func(pu ProgressUpdate) bool { return pu.Indeterminate })

	controls.AddTokens("magnum-test", 231)
	waitForProgress(t, broker, func(pu ProgressUpdate) bool { return pu.Description == "magnum-test: 231 tokens generated" })

	// Let a couple of ETA ticks pass; the task is still holding the slot.
	time.Sleep(2100 * time.Millisecond)
	pkt, ok := broker.State(events.PROGRESS_UPDATE).(events.Packet)
	if !ok {
		t.Fatal("no progress packet cached")
	}
	if pu, ok := pkt.Data.(ProgressUpdate); !ok || pu.Description != "magnum-test: 231 tokens generated" {
		t.Errorf("expected the token line to survive the ETA ticks, got %+v", pkt.Data)
	}
}

// The task line keeps the model next to the token count; a credit without a
// model name (an older llama-swap row) still reads sensibly.
func TestTokenLine(t *testing.T) {
	if got := tokenLine("magnum-v4-12b-Q8_0-HF", 231); got != "magnum-v4-12b-Q8_0-HF: 231 tokens generated" {
		t.Errorf("unexpected token line %q", got)
	}
	if got := tokenLine("", 7); got != "7 tokens generated" {
		t.Errorf("unexpected token line without a model: %q", got)
	}
}
