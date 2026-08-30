package main

import (
	"strings"
	"testing"
)

// resetACLState clears the package-level ACL maps so each test starts from a
// clean slate. They are globals shared across tests, and putACL/loadACL only
// ever append to them.
func resetACLState() {
	whitelist = map[string]map[string][]string{}
	blacklist = map[string]map[string][]string{}
	fullaccess = map[string]struct{}{}
}

// assertStoredEntry verifies that list[login][domain] contains path.
func assertStoredEntry(t *testing.T, name string, list map[string]map[string][]string, login, domain, path string) {
	t.Helper()
	paths, ok := list[login][domain]
	if !ok {
		t.Fatalf("%s[%q][%q] missing, login has %v", name, login, domain, list[login])
	}
	for _, p := range paths {
		if p == path {
			return
		}
	}
	t.Errorf("%s[%q][%q] = %v, want to contain %q", name, login, domain, paths, path)
}

// TestPutACL checks that putACL stores the right domain/path for a service and
// rejects unknown services.
func TestPutACL(t *testing.T) {
	t.Run("stores whitelist entry", func(t *testing.T) {
		defer resetACLState()
		if err := putACL("alice", "status"); err != nil {
			t.Fatalf("putACL = %v", err)
		}
		// "status" maps to domain "" and path "/q"
		assertStoredEntry(t, "whitelist", whitelist, "alice", "", "/q")
	})

	t.Run("stores domain-scoped service with dot suffix", func(t *testing.T) {
		defer resetACLState()
		if err := putACL("bob", "acestep"); err != nil {
			t.Fatalf("putACL = %v", err)
		}
		// "acestep" maps to domain "acestep" and path "/"; stored domain gains "."
		assertStoredEntry(t, "whitelist", whitelist, "bob", "acestep.", "/")
	})

	t.Run("stores blacklist entry", func(t *testing.T) {
		defer resetACLState()
		if err := putACL("charlie", "-status"); err != nil {
			t.Fatalf("putACL = %v", err)
		}
		assertStoredEntry(t, "blacklist", blacklist, "charlie", "", "/q")
	})

	t.Run("accumulates multiple services for one login", func(t *testing.T) {
		defer resetACLState()
		for _, svc := range []string{"comfyui", "tts"} {
			if err := putACL("eve", svc); err != nil {
				t.Fatalf("putACL(%q) = %v", svc, err)
			}
		}
		assertStoredEntry(t, "whitelist", whitelist, "eve", "", "/cui")
		assertStoredEntry(t, "whitelist", whitelist, "eve", "", "/tts")
	})

	t.Run("rejects unknown service", func(t *testing.T) {
		defer resetACLState()
		err := putACL("dave", "unknown-service")
		if err == nil {
			t.Fatal("expected error for unknown service")
		}
		if !strings.Contains(err.Error(), "unknown-service") {
			t.Errorf("error = %q, want it to mention the service name", err.Error())
		}
	})
}

// TestCheckACL exercises every branch of checkACL: allow-all, fullaccess,
// whitelist match/mismatch, domain scoping, and blacklist deny.
func TestCheckACL(t *testing.T) {
	tests := []struct {
		name   string
		setup  func()
		domain string
		path   string
		login  string
		want   bool
	}{
		{
			name:   "no acl at all allows everything",
			setup:  func() {},
			domain: "",
			path:   "/anything",
			login:  "anyuser",
			want:   true,
		},
		{
			name: "fullaccess grants access to any domain/path",
			setup: func() {
				putACL("someone", "status") // force acl to be loaded (non-empty maps)
				fullaccess["admin"] = struct{}{}
			},
			domain: "example.com",
			path:   "/anything",
			login:  "admin",
			want:   true,
		},
		{
			name: "user absent from whitelist is denied once acl is loaded",
			setup: func() {
				putACL("alice", "status")
			},
			domain: "",
			path:   "/q",
			login:  "bob",
			want:   false,
		},
		{
			name: "whitelist exact path match",
			setup: func() { putACL("u", "status") }, // path "/q"
			path:  "/q",
			login: "u",
			want:  true,
		},
		{
			name: "whitelist path is a prefix of the request",
			setup: func() { putACL("u", "status") }, // path "/q"
			path:  "/q/status.json",
			login: "u",
			want:  true,
		},
		{
			name: "path outside the whitelist is denied",
			setup: func() { putACL("u", "status") }, // path "/q"
			path:  "/other",
			login: "u",
			want:  false,
		},
		{
			name: "domain-scoped service matches its own domain",
			setup: func() { putACL("u", "acestep") }, // domain "acestep."
			domain: "acestep.",
			path:   "/anything",
			login:  "u",
			want:   true,
		},
		{
			name: "domain-scoped service denies a different domain",
			setup: func() { putACL("u", "acestep") }, // domain "acestep."
			domain: "other.",
			path:   "/anything",
			login:  "u",
			want:   false,
		},
		{
			name: "blacklist alone grants nothing (no whitelist entry)",
			setup: func() { putACL("u", "-status") }, // blacklist only
			path:  "/q",
			login: "u",
			want:  false,
		},
		{
			// The blacklist deny branch: user is broadly whitelisted (a1111 -> "/")
			// and has a blacklisted sub-path (status -> "/q").
			name: "blacklist carves out a sub-path of a broad whitelist",
			setup: func() {
				putACL("u", "a1111") // path "/"
				putACL("u", "-status") // blacklist path "/q"
			},
			path:  "/q/status.json",
			login: "u",
			want:  false,
		},
		{
			name: "blacklist does not affect an unrelated whitelisted path",
			setup: func() {
				putACL("u", "a1111") // path "/"
				putACL("u", "-status") // blacklist path "/q"
			},
			path:  "/elsewhere",
			login: "u",
			want:  true,
		},
		{
			name: "path with trailing slash under a whitelisted prefix",
			setup: func() { putACL("u", "comfyui") }, // path "/cui"
			path:  "/cui/",
			login: "u",
			want:  true,
		},
		{
			name: "deeper path under a whitelisted prefix",
			setup: func() { putACL("u", "comfyui") }, // path "/cui"
			path:  "/cui/api/status",
			login: "u",
			want:  true,
		},
		{
			// Documents the prefix-matching quirk: matching is by string prefix,
			// with no boundary check, so "/cui_extra" matches the "/cui" prefix.
			name:  "string prefix has no path-boundary check",
			setup: func() { putACL("u", "comfyui") }, // path "/cui"
			path:  "/cui_extra",
			login: "u",
			want:  true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			resetACLState()
			tt.setup()
			got := checkACL(tt.domain, tt.path, tt.login)
			if got != tt.want {
				t.Errorf("checkACL(%q, %q, %q) = %v, want %v\nwhitelist=%v\nblacklist=%v\nfullaccess=%v",
					tt.domain, tt.path, tt.login, got, tt.want, whitelist, blacklist, fullaccess)
			}
		})
	}
}

// TestLoadACL checks that loadACL builds fullaccess/whitelist/blacklist from
// config.ACL. It snapshots and restores config.ACL so it does not leak state.
func TestLoadACL(t *testing.T) {
	oldACL := config.ACL
	config.ACL = ACL{
		"fulluser": {"*"},
		"limited":  {"status", "tts"},
		"mixed":    {"a1111", "-status"},
	}
	resetACLState() // loadACL appends to the package maps and never clears them
	defer func() {
		config.ACL = oldACL
		resetACLState()
	}()

	if err := loadACL(); err != nil {
		t.Fatalf("loadACL() error = %v", err)
	}

	// Full access user ("*")
	if _, ok := fullaccess["fulluser"]; !ok {
		t.Error("fulluser should be registered in fullaccess")
	}
	if !checkACL("example.com", "/anything", "fulluser") {
		t.Error("fulluser should have access to everything")
	}

	// Limited user: whitelisted for /q and /tts only
	if !checkACL("", "/q", "limited") {
		t.Error("limited should be allowed on /q")
	}
	if !checkACL("", "/tts", "limited") {
		t.Error("limited should be allowed on /tts")
	}
	if checkACL("", "/cui", "limited") {
		t.Error("limited should NOT be allowed on /cui")
	}

	// Mixed user: broad whitelist "/" minus blacklisted "/q"
	if !checkACL("", "/elsewhere", "mixed") {
		t.Error("mixed should be allowed on a non-blacklisted path")
	}
	if checkACL("", "/q", "mixed") {
		t.Error("mixed should be denied on blacklisted /q")
	}
}
