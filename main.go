package main

import (
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
}

var domains = map[string]echo.MiddlewareFunc{
	"":            proxy.NewProxyWrapperStr(SD_URL, nil),
	"acestep.":    proxy.NewProxyWrapperStr(AS10_URL, nil),
	"as15.":       proxy.NewProxyWrapperStr(AS15_URL, nil),
	"ovi.":        proxy.NewProxyWrapperStr(OVI_URL, nil),
	"cui.":        proxy.NewProxyWrapperStr(CUI_URL, nil),
	"/vote2025hw": proxy.NewProxyWrapperStr(SDVOTE_URL, nil),
}

var skipAuth = map[string][]string{
	"path": {
		"/login", "/metrics", "/internal/join", "/internal/leave", "/internal/free_complete", "/cui/join", "/cui/leave", "/cui/progress", "/acestep/join", "/acestep/leave", "/acestep15/join", "/acestep15/leave", "/ovi/join", "/ovi/leave",
	},
	"prefix": {
		"/v1/", "/sdapi/",
	},
}

func post(path string) {
	_, err := http.Post(SD_URL+path, "", nil)
	if err != nil {
		log.Printf("*** Error calling %s: %s", path, err)
		return
	}
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
			for _, p := range skipAuth["path"] {
				if path == p {
					return true
				}
			}
			for _, p := range skipAuth["prefix"] {
				if strings.HasPrefix(path, p) {
					return true
				}
			}
			return false
		},
	}))
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
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
	wd := watchdog.NewWatchdog(config.FIFOPath)
	svcChan := make(chan servicequeue.SvcUpdate)
	sq := servicequeue.NewServiceQueue(svcChan)
	e.POST("/internal/free_complete", func(c echo.Context) error {
		sq.SetCleanupProgress(true)
		return nil
	})
	pr := progress.NewProgress(broker, SD_URL, config.SDTimeout, wd, mchan, svcChan)
	pr.AddHandlers(e)
	pr.Start(sq)
	llmurl, err := url.Parse(LLM_URL)
	if err != nil {
		log.Fatal(err)
	}
	e.Group("/*", earlyCheckMiddleware("/"), func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			for d, t := range domains {
				if len(d) > 0 && d[0] == '/' { // skip path checks
					continue
				}
				if c.Request().Host == d+config.Domain {
					return t(next)(c)
				}
			}
			return domains[""](next)(c)
		}
	})
	for d, t := range domains {
		if len(d) > 0 && d[0] == '/' {
			trail := middleware.AddTrailingSlashWithConfig(middleware.TrailingSlashConfig{RedirectCode: http.StatusMovedPermanently, Skipper: func(c echo.Context) bool {
				return c.Request().RequestURI != d
			}})
			e.Group(d, earlyCheckMiddleware(d), trail, middleware.Rewrite(map[string]string{d + "/*": "/$1"}), t)
		}
	}
	e.Group("/sdapi", domains[config.Domain])
	addSDQueueHandlers(e, sq)
	addASQueueHandlers(e, sq)
	addOviQueueHandlers(e, sq)
	if llmurl.Scheme != "" {
		llm := NewLLMBalancer(llmurl, sq, mchan)
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
		cuiurl, err := url.Parse(CUI_URL)
		if err != nil {
			log.Fatalf("Error parsing CUI URL: %s", err)
		}
		addCUIHandlers(e, sq, cuiurl)
		e.Group("/cui/*", earlyCheckMiddleware("/cui/"), middleware.Rewrite(map[string]string{"/cui/*": "/$1"}), newCUIProxy(cuiurl))
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
