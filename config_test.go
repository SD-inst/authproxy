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

// VIP keys are configured as digests too, and they decide who may hold the
// service slot between requests, so a malformed entry must match nothing.
func TestLLMVIPKeys(t *testing.T) {
	oldKeys, oldPeriod := config.LLMVIPKeys, config.LLMVIPGracePeriod
	oldVip, oldVipPeriod := vipKeys, vipGracePeriod
	defer func() {
		config.LLMVIPKeys, config.LLMVIPGracePeriod = oldKeys, oldPeriod
		vipKeys, vipGracePeriod = oldVip, oldVipPeriod
	}()

	config.LLMVIPKeys = []string{
		strings.ToUpper(keyDigestHex("sk-vip")), // digests are case-insensitive
		keyDigestHex("Bearer sk-vip-header"),    // configured over the header as sent
		"sk-not-a-digest",                       // malformed: must be skipped
		"  ",
	}
	config.LLMVIPGracePeriod = "30s"
	config.buildVipKeys()

	if !isVIPKey("sk-vip") {
		t.Fatal("the bare-key digest must mark the key VIP")
	}
	if !isVIPKey("Bearer sk-vip") {
		t.Fatal("the Bearer-prefixed form must match the same digest")
	}
	if !isVIPKey("Bearer sk-vip-header") {
		t.Fatal("the header-form digest must match")
	}
	if isVIPKey("sk-other") {
		t.Fatal("an unconfigured key must not be VIP")
	}
	if isVIPKey("sk-not-a-digest") {
		t.Fatal("a malformed digest entry must match nothing")
	}
	if vipGracePeriod != 30*time.Second {
		t.Fatalf("expected the configured 30s hold, got %s", vipGracePeriod)
	}

	// Unset or unparseable period falls back to the default; a negative one too.
	for _, s := range []string{"", "notaduration", "-5s"} {
		config.LLMVIPGracePeriod = s
		config.buildVipKeys()
		if vipGracePeriod != defaultVIPGracePeriod {
			t.Fatalf("expected the default hold for %q, got %s", s, vipGracePeriod)
		}
	}
	// 0 is a legitimate value: it disables the hold without dropping the keys.
	config.LLMVIPGracePeriod = "0s"
	config.buildVipKeys()
	if vipGracePeriod != 0 {
		t.Fatalf("expected a zero hold, got %s", vipGracePeriod)
	}

	// No keys configured: nobody holds the slot.
	config.LLMVIPKeys = nil
	config.buildVipKeys()
	if isVIPKey("sk-vip") {
		t.Fatal("without llm_vip_keys no key may be VIP")
	}
}
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
