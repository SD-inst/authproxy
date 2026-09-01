package main

import (
	"slices"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/go-flags"
	"github.com/golang-jwt/jwt/v5"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/rkfg/authproxy/events"
	"github.com/rkfg/authproxy/metrics"
	"github.com/rkfg/authproxy/progress"
	"github.com/rkfg/authproxy/proxy"
	"github.com/rkfg/authproxy/servicequeue"
	"github.com/rkfg/authproxy/upload"
	"github.com/rkfg/authproxy/watchdog"
	"golang.org/x/crypto/bcrypt"
)

var params struct {
	ConfigFilename string `short:"c" description:"Config filename" required:"true"`
	AddUser        bool   `short:"a" description:"Add new user"`
	Username       string `short:"u" description:"Username for -a"`
	Password       string `short:"p" description:"Password for -a"`
	JWTSecret      string
	Watchdog       string `long:"watchdog" description:"Watchdog control address (ip:port); enables container auto start/stop"`
	StopTimeoutMin int    `long:"stop-timeout" default:"60" description:"Minutes of inactivity before a container is auto-stopped"`
	DowntimeFile   string `long:"downtime-file" description:"Path to the downtime status file; a JSON object with a \"started\" field enables downtime mode (no container starts, services return 502)"`
}

var domains = map[string]echo.MiddlewareFunc{
	"":               proxy.NewProxyWrapperStr(SD_URL, nil),
	"acestep.":       proxy.NewProxyWrapperStr(AS10_URL, nil),
	"as15.":          proxy.NewProxyWrapperStr(AS15_URL, nil),
	"ovi.":           proxy.NewProxyWrapperStr(OVI_URL, nil),
	"cui.":           proxy.NewProxyWrapperStr(CUI_URL, nil),
	"vlo.":           proxy.NewProxyWrapperStr(VLO_URL, nil),
	"/vote2025hw":    proxy.NewProxyWrapperStr(SDVOTE_URL, nil),
	"/lora_previews": proxy.NewProxyWrapperStr(CADDY_URL, nil),
}

var skipAuth = map[string][]string{
	"path": {
		"/login", "/metrics", "/internal/join", "/internal/leave", "/internal/free_complete", "/cui/join", "/cui/leave", "/cui/progress", "/acestep/join", "/acestep/leave", "/acestep15/join", "/acestep15/leave", "/ovi/join", "/ovi/leave", "/q/status.json",
	},
	"prefix": {
		"/v1/", "/sdapi/",
	},
}

// proxyAuthHeader is the header Caddy sets (via header_up) on routes already
// gated by Caddy-side basic auth or a secret URL. The value is a server-only
// shared secret, so a client cannot forge a valid bypass header.
const proxyAuthHeader = "X-Proxy-Auth"

// isProxyAuth reports whether the request carries the Caddy-side shared
// proxy-auth header with the configured secret value. When the secret is
// unset, it is always false (feature disabled).
func isProxyAuth(c echo.Context) bool {
	if config.ProxyAuthSecret == "" {
		return false
	}
	h := c.Request().Header.Get(proxyAuthHeader)
	if h == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h), []byte(config.ProxyAuthSecret)) == 1
}

func post(path string) {
	_, err := http.Post(SD_URL+path, "", nil)
	if err != nil {
		log.Printf("*** Error calling %s: %s", path, err)
		return
	}
}

// loadDowntime reports whether the downtime status file at path marks an active
// downtime. Normal operation (false) is the default in every case: the file is
// unset, missing, empty, not a JSON object, or an object without a "started"
// field. Only a JSON object carrying a "started" field switches to downtime.
func loadDowntime(path string) bool {
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Cannot read downtime file %s: %s; running normally", path, err)
		}
		return false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return false
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		log.Printf("Downtime file %s is not a JSON object (%s); running normally", path, err)
		return false
	}
	if _, ok := obj["started"]; !ok {
		return false
	}
	return true
}

func main() {
	_, err := flags.Parse(&params)
	if err != nil {
		return
	}
	if err = loadConfig(params.ConfigFilename); err != nil {
		log.Fatal(err)
	}
	if params.AddUser {
		if params.Username == "" || params.Password == "" {
			log.Fatal("Specify username and password to add")
		}
		hashed, err := bcrypt.GenerateFromPassword([]byte(params.Password), bcrypt.DefaultCost)
		if err != nil {
			log.Fatalf("Error hashing password: %s", err)
		}
		err = loadCreds(config.CredFilename)
		if err != nil {
			log.Printf("Error loading existing users: %s, will create a new file and JWT secret", err)
			r := rand.New(rand.NewSource(time.Now().UnixNano()))
			params.JWTSecret = randomString(r, 64)
		}
		creds[strings.ToLower(params.Username)] = string(hashed)
		saveCreds(config.CredFilename)
		log.Printf("User %s added", params.Username)
		return
	}
	err = loadCreds(config.CredFilename)
	if err != nil {
		log.Fatal(err)
	}
	err = loadACL()
	if err != nil {
		log.Fatalf("Error loading ACL: %s", err)
	}
	e := echo.New()
	mchan := metrics.NewMetrics(e, config.PushPassword)
	e.Use(echojwt.WithConfig(echojwt.Config{
		SigningKey:   []byte(params.JWTSecret),
		ErrorHandler: keyErrorHandler,
		TokenLookup:  "cookie:" + cookieName,
		Skipper: func(c echo.Context) bool {
			path := c.Path()
			if slices.Contains(skipAuth["path"], path) {
					return true
				}
			for _, p := range skipAuth["prefix"] {
				if strings.HasPrefix(path, p) {
					return true
				}
			}
			// Caddy-only service routes (already gated by basic auth or a
			// secret URL) carry the shared proxy-auth header; let them through
			// without a JWT. Scoped to the service prefix so the blast radius
			// is limited to that service even if the header were ever forged.
			if isProxyAuth(c) {
				return true
			}
			return false
		},
	}))
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		Skipper: func(c echo.Context) bool {
			return c.Path() == "/q/status.json" || c.Path() == "/metrics"
		},
		LogRemoteIP:     true,
		LogURI:          true,
		LogMethod:       true,
		LogStatus:       true,
		LogUserAgent:    true,
		LogResponseSize: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			token := c.Get("user")
			user := "???"
			if token != nil {
				claims := token.(*jwt.Token).Claims
				if claims != nil {
					subject, err := claims.GetSubject()
					if err == nil && subject != "" {
						user = subject
					}
				}
			}
			log.Printf("%s %s %s %s %d %d %s", v.RemoteIP, user, v.Method, v.URI, v.Status, v.ResponseSize, v.UserAgent)
			return nil
		},
	}))
	e.Use(aclMiddleware())
	e.GET("/login", loginPageHandler)
	e.GET("/logout", logoutHandler)
	e.POST("/login", loginHandler)
	broker := events.NewBroker()
	wd := watchdog.NewWatchdog(params.Watchdog)
	m := newContainerManager(wd, time.Duration(params.StopTimeoutMin)*time.Minute)
	m.downtime = loadDowntime(params.DowntimeFile)
	if m.downtime {
		log.Printf("Downtime mode active (from %s): containers will not be started; service requests return 502", params.DowntimeFile)
	}
	if m.enabled() {
		log.Printf("Watchdog enabled at %s; auto start/stop after %s idle", params.Watchdog, m.stopAfter)
	} else {
		log.Printf("Watchdog disabled (--watchdog not set): container auto start/stop and restart-on-demand are OFF")
	}
	svcChan := make(chan servicequeue.SvcUpdate)
	sq := servicequeue.NewServiceQueue(svcChan)
	e.POST("/internal/free_complete", func(c echo.Context) error {
		sq.SetCleanupProgress(true)
		return nil
	})
	pr := progress.NewProgress(broker, SD_URL, config.SDTimeout, wd, mchan, svcChan, config.StatusToken, sq)
	pr.AddHandlers(e)
	pr.Start(sq)
	llmurl, err := url.Parse(LLM_URL)
	if err != nil {
		log.Fatal(err)
	}
	// The ComfyUI websocket is routed separately so that a reconnect while the
	// container is stopped does not start it. Precompute the real-proxy handlers
	// (the container's /ws, with or without the /cui path rewrite) here so they
	// are in scope for both the path route and the cui. domain route below.
	var cuiurl *url.URL
	var cuiWSPath, cuiWSDomain, cuiJobsPath echo.HandlerFunc
	if CUI_URL != "" {
		cuiurl, err = url.Parse(CUI_URL)
		if err != nil {
			log.Fatalf("Error parsing CUI URL: %s", err)
		}
		cuiWSDomain = composeMW(newCUIProxy(cuiurl))
		// The rewrite keys are matched against RequestURI (path + query), and
		// rewriteRulesRegex anchors them to the end of the string, so an exact
		// path never matches once a query is present — the rule would drop and
		// comfyui would get the raw path and 404. The wildcards capture the
		// query into $1 so the backend sees the real path with its args.
		cuiWSPath = composeMW(middleware.Rewrite(map[string]string{"/cui/ws*": "/ws$1"}), newCUIProxy(cuiurl))
		cuiJobsPath = composeMW(middleware.Rewrite(map[string]string{"/cui/api/jobs*": "/api/jobs$1"}), newCUIProxy(cuiurl))
	}
	e.Group("/*", earlyCheckMiddleware("/"), func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if cuiurl != nil && c.Request().Host == "cui."+config.Domain && c.Request().URL.Path == "/ws" {
				return m.cuiWSHandler(cuiWSDomain)(c)
			}
			for d, t := range domains {
				if len(d) > 0 && d[0] == '/' { // skip path checks
					continue
				}
				if c.Request().Host == d+config.Domain {
					return m.withDomain(t, d)(next)(c)
				}
			}
			return m.withDomain(domains[""], "")(next)(c)
		}
	})
	for d, t := range domains {
		if len(d) > 0 && d[0] == '/' {
			trail := middleware.AddTrailingSlashWithConfig(middleware.TrailingSlashConfig{RedirectCode: http.StatusMovedPermanently, Skipper: func(c echo.Context) bool {
				return c.Request().RequestURI != d
			}})
			e.Group(d, earlyCheckMiddleware(d), trail, t)
		}
	}
	e.Group("/sdapi", m.ensureService("stablediff-cuda"), domains[config.Domain])
	addSDQueueHandlers(e, sq)
	addASQueueHandlers(e, sq)
	addOviQueueHandlers(e, sq)
	if llmurl.Scheme != "" {
		llm := NewLLMBalancer(llmurl, sq, mchan)
		e.POST("/upstream/:model/v1/streams/lookup", llm.lookup)
		e.GET("/upstream/:model/tools", llm.tools)
		e.Group("/v1/*", llm.proxy)
		e.Group("/upstream/*", llm.proxy)
		e.POST("/v1/internal/encode", nil, llm.proxy)
		e.Any("/v1/internal/*", llm.forbidden)
		e.GET("/v1/models/*", llm.forbidden)
	}
	if config.LoRAPath != "" {
		upload.NewUploader(e.Group("/upload"), config.LoRAPath, config.CookieFile, broker, mchan)
	}
	if TTS_URL != "" {
		ttsurl, err := url.Parse(TTS_URL)
		if err != nil {
			log.Fatalf("Error parsing TTS URL: %s", err)
		}
		e.Group("/tts/*", earlyCheckMiddleware("/tts/"), middleware.Rewrite(map[string]string{"/tts/*": "/$1"}), newTTSProxy(ttsurl, sq, wd))
	}
	if CUI_URL != "" {
		addCUIHandlers(e, sq, cuiurl, pr)
		// /cui/ws is matched before the /cui/* group (static beats wildcard), so
		// the websocket never goes through ensureService and thus never starts or
		// keeps the container alive; it is served silently while the container is
		// stopped and proxied for real once it is up.
		e.Any("/cui/ws", m.cuiWSHandler(cuiWSPath), earlyCheckMiddleware("/cui/ws"))
		// /cui/api/jobs is the page's post-reconnect poll; like /cui/ws it is a
		// static route so it never reaches ensureService. Stopped → empty stub,
		// running → real proxy.
		e.Any("/cui/api/jobs", m.cuiJobsHandler(cuiJobsPath), earlyCheckMiddleware("/cui/api/jobs"))
		e.Group("/cui/*", earlyCheckMiddleware("/cui/"), m.ensureService("comfyui"), middleware.Rewrite(map[string]string{"/cui/*": "/$1"}), newCUIProxy(cuiurl))
	}
	if config.StaticPath != "" {
		dirs, err := os.ReadDir(config.StaticPath)
		if err != nil {
			log.Fatalf("Error reading static directory %s: %s", config.StaticPath, err)
		}
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			dirname := d.Name()
			e.Group("/"+dirname, earlyCheckMiddleware("/"+dirname+"/"), middleware.AddTrailingSlashWithConfig(middleware.TrailingSlashConfig{RedirectCode: 302, Skipper: func(c echo.Context) bool {
				return c.Path() != "/"+dirname
			}}), middleware.Static(filepath.Join(config.StaticPath, dirname)))
		}
	}
	err = e.Start(config.Address)
	if err != nil {
		log.Fatal(err)
	}
}
