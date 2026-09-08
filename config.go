package main

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// UNAUTH_OWNER is the sentinel owner the services send when no authenticated
// user header is present (progress.py / ComfyUI main.py). taskLimit maps it to
// TaskTimeoutDefault. Keep it distinct from any real username.
const UNAUTH_OWNER = "default"

type ACL map[string][]string

type Config struct {
	CredFilename string `yaml:"accounts" description:"Credentials filename" required:"true"`
	Domain       string `yaml:"domain" description:"Main domain"`
	Address      string `yaml:"address" description:"Listen at this address"`
	LoRAPath     string `yaml:"lora_uploads" description:"Path to the directory for LoRA uploads"`
	LoginHeader  string `yaml:"login_header" description:"Title text for login page"`
	LoginTitle   string `yaml:"login_title" description:"Login page invitation text"`
	SDTimeout    int    `yaml:"sd_timeout" description:"SD task timeout in seconds"`
	// TaskTimeout holds the per-user task time limit (Go duration string, e.g.
	// "4m"). It overrides TaskTimeoutDefault for the named user. A user not in
	// the map (or an empty/unparseable value) falls back to TaskTimeoutDefault.
	TaskTimeout map[string]string `yaml:"task_timeout" description:"Per-user task time limit (Go duration, e.g. '4m'); overrides task_timeout_default"`
	TaskTimeoutDefault string     `yaml:"task_timeout_default" description:"Default task time limit for users without a specific entry (Go duration, e.g. '10m')"`
	// TaskMaxLifetime is the safety upper bound on a tracked job: if a job is
	// still active after this long (lost job_end / container restart) it is
	// cleared so the timer cannot leak. Default "2h" when unset.
	TaskMaxLifetime string `yaml:"task_max_lifetime" description:"Safety upper bound on a tracked job before it is cleared (Go duration, default '2h')" `
	CookieFile   string `yaml:"cookie_file" description:"Path to the cookie storage file"`
	PushPassword string `yaml:"push_password" description:"Password to push prometheus metrics from other services"`
	StaticPath   string `yaml:"static_path" description:"Path to the static pages (each dir will be available at corresponding /dir URL)"`
	ACL          ACL    `yaml:"acl,flow" description:"Mapping of user names to a list or roles or * for full access"`
	StatusToken  string `yaml:"status_token" description:"Token for /q/status.json endpoint auth"`
	// ProxyAuthSecret is the shared secret Caddy sets in the proxyAuthHeader
	// on routes already gated by Caddy-side basic auth or a secret URL. A
	// request carrying it on a service path bypasses the JWT check. The value
	// is server-only (never sent to or visible to a browser), so a client
	// cannot forge a valid bypass header.
	ProxyAuthSecret string `yaml:"proxy_auth_secret" description:"Shared secret Caddy sends to bypass JWT on gated service routes"`
	// IPBlacklist is a list of client IPs or CIDR ranges to drop at the
	// connection level. The client IP is taken from the proxy headers
	// (X-Forwarded-For / X-Real-IP) since the physical peer is the upstream
	// proxy. A matching request gets its connection severed immediately, with
	// no HTTP status line or response headers sent.
	IPBlacklist []string `yaml:"ip_blacklist" description:"Client IPs or CIDR ranges to drop immediately (connection closed, no response)"`
}

var config = Config{
	Address:     "0.0.0.0:8000",
	LoginHeader: "Stable Diffusion for friends",
	LoginTitle:  "Please log in",
	SDTimeout:   300,
	CookieFile:  "cookie.txt",
}

func loadConfig(filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	return yaml.NewDecoder(f).Decode(&config)
}

// taskLimit returns the per-user task time limit. Unauthenticated owners (an
// empty name, or the UNAUTH_OWNER sentinel the services send when no user header
// is present) use TaskTimeoutDefault. A named user uses TaskTimeout[username] if
// set, else TaskTimeoutDefault. It reports ok=false (no limit) only when the
// resolved value is empty or unparseable, so the caller does not enforce a limit.
func taskLimit(username string) (time.Duration, bool) {
	var s string
	if username == "" || username == UNAUTH_OWNER {
		s = config.TaskTimeoutDefault
	} else {
		s = config.TaskTimeout[username]
		if s == "" {
			s = config.TaskTimeoutDefault
		}
	}
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// taskMaxLifetime returns the safety upper bound on a tracked job, defaulting to
// 2h when unset or unparseable.
func (c *Config) taskMaxLifetime() time.Duration {
	if c.TaskMaxLifetime != "" {
		if d, err := time.ParseDuration(c.TaskMaxLifetime); err == nil && d > 0 {
			return d
		}
	}
	return 2 * time.Hour
}
