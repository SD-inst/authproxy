package main

import (
	"testing"
	"time"
)

func TestTaskLimit(t *testing.T) {
	oldMap := config.TaskTimeout
	oldDefault := config.TaskTimeoutDefault
	config.TaskTimeout = map[string]string{"alice": "5m"}
	config.TaskTimeoutDefault = "10m"
	defer func() {
		config.TaskTimeout = oldMap
		config.TaskTimeoutDefault = oldDefault
	}()

	// Per-user entry wins.
	d, ok := taskLimit("alice")
	if !ok || d != 5*time.Minute {
		t.Fatalf("expected alice -> 5m, got (%s, %v)", d, ok)
	}
	// Unknown user falls back to the default.
	d, ok = taskLimit("bob")
	if !ok || d != 10*time.Minute {
		t.Fatalf("expected bob -> 10m default, got (%s, %v)", d, ok)
	}
	// Empty default -> no limit.
	config.TaskTimeoutDefault = ""
	_, ok = taskLimit("bob")
	if ok {
		t.Fatalf("expected no limit when default is empty")
	}
	// Unparseable value -> no limit.
	config.TaskTimeout = map[string]string{"carol": "notaduration"}
	config.TaskTimeoutDefault = ""
	_, ok = taskLimit("carol")
	if ok {
		t.Fatalf("expected no limit for unparseable value")
	}
}
