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

func TestTaskColor(t *testing.T) {
	oldColors := config.TaskColors
	defer func() {
		config.TaskColors = oldColors
		config.buildUserColors()
	}()

	config.TaskColors = map[string][]string{
		"#a855f7": {"carol", "User2"},
		"#ff7a1a": {"alice", "bob"},
		"#12345":  {" alice "},
		"#00ff00": {"alice", "spaced "},
		"red":     {"whoever"}, // invalid color form
	}
	config.buildUserColors()

	// A user listed under several colors keeps the alphabetically first one.
	if got := taskColor("alice"); got != "#00ff00" {
		t.Fatalf("expected alice -> #00ff00, got %q", got)
	}
	if got := taskColor("BOB"); got != "#ff7a1a" {
		t.Fatalf("expected case-insensitive lookup, got %q", got)
	}
	if got := taskColor("user2"); got != "#a855f7" {
		t.Fatalf("expected user2 -> #a855f7, got %q", got)
	}
	// Usernames are trimmed on both sides.
	if got := taskColor("spaced"); got != "#00ff00" {
		t.Fatalf("expected trimmed user to keep its color, got %q", got)
	}
	// Invalid color form: the user gets no color.
	if got := taskColor("whoever"); got != "" {
		t.Fatalf("expected no color for an invalid color entry, got %q", got)
	}
	// Unknown and unauthenticated users get the default (no color).
	if got := taskColor("nobody"); got != "" {
		t.Fatalf("expected no color for unknown user, got %q", got)
	}
	if got := taskColor(UNAUTH_OWNER); got != "" {
		t.Fatalf("expected no color for the unauthenticated owner, got %q", got)
	}
	if got := taskColor(""); got != "" {
		t.Fatalf("expected no color for an empty owner, got %q", got)
	}
}
