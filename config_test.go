package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
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

// keyDigestHex is the digest form an llm_key_colors entry is written in.
func keyDigestHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestLLMKeyColor(t *testing.T) {
	oldColors := config.LLMKeyColors
	defer func() {
		config.LLMKeyColors = oldColors
		config.buildKeyColors()
	}()

	config.LLMKeyColors = map[string][]string{
		// Digests are matched case-insensitively.
		"#a855f7": {strings.ToUpper(keyDigestHex("sk-carol"))},
		// Configured over the header as the client sends it, scheme included.
		"#22c55e": {keyDigestHex("Bearer sk-dave")},
		"#12345":  {keyDigestHex("sk-badcolor")}, // invalid color form
		"#f43f5e": {"sk-not-a-digest", "  "},     // malformed entries
		"#00ff00": {keyDigestHex("sk-alice")},
		"#ff7a1a": {keyDigestHex("sk-alice")}, // duplicate: first color wins
	}
	config.buildKeyColors()

	// The documented form: the bare key, with or without the "Bearer " scheme.
	if got := llmKeyColor("sk-carol"); got != "#a855f7" {
		t.Fatalf("expected sk-carol -> #a855f7, got %q", got)
	}
	if got := llmKeyColor("Bearer sk-carol"); got != "#a855f7" {
		t.Fatalf("expected the Bearer-prefixed form to match, got %q", got)
	}
	// A digest configured over the header as sent (scheme included) matches.
	if got := llmKeyColor("Bearer sk-dave"); got != "#22c55e" {
		t.Fatalf("expected the header-form digest to match, got %q", got)
	}
	// A key listed under several colors keeps the alphabetically first one.
	if got := llmKeyColor("sk-alice"); got != "#00ff00" {
		t.Fatalf("expected the alphabetically first color, got %q", got)
	}
	// An invalid color form gives its keys no color.
	if got := llmKeyColor("sk-badcolor"); got != "" {
		t.Fatalf("expected no color for an invalid color entry, got %q", got)
	}
	// A malformed config entry matches nothing (the raw key is not an identity).
	if got := llmKeyColor("sk-not-a-digest"); got != "" {
		t.Fatalf("expected no color for a malformed digest entry, got %q", got)
	}
	// Unconfigured and empty keys get the default (no color).
	if got := llmKeyColor("sk-nobody"); got != "" {
		t.Fatalf("expected no color for an unconfigured key, got %q", got)
	}
	if got := llmKeyColor(""); got != "" {
		t.Fatalf("expected no color for an empty header, got %q", got)
	}
}
