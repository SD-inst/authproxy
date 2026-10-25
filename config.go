package main

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
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
	TaskTimeout        map[string]string `yaml:"task_timeout" description:"Per-user task time limit (Go duration, e.g. '4m'); overrides task_timeout_default"`
	TaskTimeoutDefault string            `yaml:"task_timeout_default" description:"Default task time limit for users without a specific entry (Go duration, e.g. '10m')"`
	// TaskMaxLifetime is the safety upper bound on a tracked job: if a job is
	// still active after this long (lost job_end / container restart) it is
	// cleared so the timer cannot leak. Default "2h" when unset.
	TaskMaxLifetime string `yaml:"task_max_lifetime" description:"Safety upper bound on a tracked job before it is cleared (Go duration, default '2h')" `
	CookieFile      string `yaml:"cookie_file" description:"Path to the cookie storage file"`
	PushPassword    string `yaml:"push_password" description:"Password to push prometheus metrics from other services"`
	StaticPath      string `yaml:"static_path" description:"Path to the static pages (each dir will be available at corresponding /dir URL)"`
	ACL             ACL    `yaml:"acl,flow" description:"Mapping of user names to a list or roles or * for full access"`
	StatusToken     string `yaml:"status_token" description:"Token for /q/status.json endpoint auth"`
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
	// TaskColors maps a hex color to the users whose tasks render the status
	// page (/q) progress bar as an animated gradient in that color. A color
	// may cover several users; a user listed under several colors keeps the
	// alphabetically first one. Users without an entry (and anonymous tasks)
	// get the default solid blue.
	TaskColors map[string][]string `yaml:"task_colors" description:"Status page progress bar colors: hex color (#rgb/#rrggbb) -> list of usernames"`
	// LLMKeyColors is the LLM equivalent of TaskColors. LLM clients carry an
	// API key in the Authorization header instead of a JWT, so there is no
	// username to map: the list holds the SHA-256 digests of the keys (the
	// same form LLM_PROXY_VIP_KEYS uses), never the keys themselves. A digest
	// of the bare key and a digest of the header as the client sends it both
	// match. Keys without an entry get the default solid blue.
	LLMKeyColors map[string][]string `yaml:"llm_key_colors" description:"Status page progress bar colors by LLM API key: hex color (#rgb/#rrggbb) -> list of SHA-256 key digests"`
}

// userColors is the inverted TaskColors map (username -> color), rebuilt by
// loadConfig. Kept lowercase-keyed because credential usernames are.
var userColors = map[string]string{}

// keyColors is the inverted LLMKeyColors map (lowercase hex key digest ->
// color), rebuilt by loadConfig.
var keyColors = map[string]string{}

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
	if err := yaml.NewDecoder(f).Decode(&config); err != nil {
		return err
	}
	config.buildUserColors()
	config.buildKeyColors()
	return nil
}

// cssHexColorRE accepts the short and long CSS hex color forms. Only these are
// allowed so the value can be embedded into page CSS without escaping.
var cssHexColorRE = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// keyDigestRE matches a 64-hex-digit SHA-256 digest, the form an llm_key_colors
// entry must be in (the raw key never goes into the config file).
var keyDigestRE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// sortedColors returns the keys of a color -> list mapping in sorted order, so
// a name (user or key digest) listed under several colors deterministically
// keeps the alphabetically first one.
func sortedColors(m map[string][]string) []string {
	colors := make([]string, 0, len(m))
	for color := range m {
		colors = append(colors, color)
	}
	sort.Strings(colors)
	return colors
}

// buildUserColors inverts TaskColors (color -> users) into userColors
// (username -> color). Invalid colors are skipped with a log line. Colors are
// walked in sorted order so a user listed under several colors deterministically
// keeps the alphabetically first one.
func (c *Config) buildUserColors() {
	userColors = map[string]string{}
	for _, color := range sortedColors(c.TaskColors) {
		if !cssHexColorRE.MatchString(color) {
			log.Printf("task_colors: ignoring invalid color %q (use #rgb or #rrggbb)", color)
			continue
		}
		for _, user := range c.TaskColors[color] {
			user = strings.ToLower(strings.TrimSpace(user))
			if user == "" {
				continue
			}
			if _, dup := userColors[user]; dup {
				log.Printf("task_colors: user %q listed under several colors, keeping %s", user, userColors[user])
				continue
			}
			userColors[user] = color
		}
	}
}

// buildKeyColors inverts LLMKeyColors (color -> key digests) into keyColors
// (lowercase hex digest -> color), skipping invalid colors and digests with a
// log line. Sorted color order gives a digest listed under several colors the
// alphabetically first one.
func (c *Config) buildKeyColors() {
	keyColors = map[string]string{}
	for _, color := range sortedColors(c.LLMKeyColors) {
		if !cssHexColorRE.MatchString(color) {
			log.Printf("llm_key_colors: ignoring invalid color %q (use #rgb or #rrggbb)", color)
			continue
		}
		for _, digest := range c.LLMKeyColors[color] {
			digest = strings.ToLower(strings.TrimSpace(digest))
			if digest == "" {
				continue
			}
			if !keyDigestRE.MatchString(digest) {
				log.Printf("llm_key_colors: ignoring invalid key digest %q (want 64-hex-digit SHA-256)", digest)
				continue
			}
			if _, dup := keyColors[digest]; dup {
				log.Printf("llm_key_colors: key digest %q listed under several colors, keeping %s", digest, keyColors[digest])
				continue
			}
			keyColors[digest] = color
		}
	}
}

// taskColor returns the status-page progress bar color configured for a
// username ("" = the default blue). Unauthenticated owners get the default.
func taskColor(username string) string {
	if username == "" || username == UNAUTH_OWNER {
		return ""
	}
	return userColors[strings.ToLower(username)]
}

// apiKeyVariants returns the forms of an Authorization header value a key can
// be configured by: the bare key (the documented form — the optional "Bearer "
// scheme is stripped) and, when the header carried a scheme, the header value
// as the client sent it. An empty header has no variants.
func apiKeyVariants(header string) []string {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	bare := header
	// The scheme is not part of the key (HTTP auth schemes are case-insensitive),
	// so the bare key is a variant of its own.
	if scheme, rest, ok := strings.Cut(header, " "); ok && strings.EqualFold(scheme, "Bearer") {
		bare = strings.TrimSpace(rest)
	}
	variants := []string{bare}
	if bare != header {
		variants = append(variants, header)
	}
	return variants
}

// llmKeyColor returns the status-page progress bar color configured for the
// LLM API key in an Authorization header value ("" = the default blue). LLM
// clients authenticate with a key rather than a JWT, so the key's digest is
// the lookup identity; the key itself is never sent to the page. Both the
// digest of the bare key and the digest of the header as the client sent it
// match.
func llmKeyColor(authorization string) string {
	for _, variant := range apiKeyVariants(authorization) {
		sum := sha256.Sum256([]byte(variant))
		if color := keyColors[hex.EncodeToString(sum[:])]; color != "" {
			return color
		}
	}
	return ""
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
