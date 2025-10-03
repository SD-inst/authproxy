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
	CredFilename string `short:"f" description:"Credentials filename" required:"true"`
	AddUser      bool   `short:"a" description:"Add new user"`
	Username     string `short:"u" description:"Username for -a"`
	Password     string `short:"p" description:"Password for -a"`
	Domain       string `short:"d" description:"Main domain"`
	LLMURL       string `long:"llm-url" description:"Target LLM URL to proxy to"`
	TTSURL       string `long:"tts-url" description:"TTS URL"`
	CUIURL       string `long:"cui-url" description:"ComfyUI URL to proxy to"`
	Address      string `short:"l" description:"Listen at this address" default:"0.0.0.0:8000"`
	LoRAPath     string `long:"lora-path" description:"Path to the directory for LoRA uploads"`
	SDHost       string `long:"sd-host" description:"Stable Diffusion host to monitor" default:"http://stablediff-cuda:7860"`
	SDTimeout    int    `long:"sd-timeout" description:"SD task timeout in seconds" default:"300"`
	FIFOPath     string `long:"fifo-path" description:"Path to FIFO controlling instance restarts" default:"/var/run/sdwd/control.fifo"`
	JWTSecret    string
	CookieFile   string `long:"cookie-file" description:"Path to the cookie storage file"`
	PushPassword string `long:"push-password" description:"Password to push prometheus metrics from other services"`
	StaticPath   string `long:"static-path" description:"Path to the static pages (each dir will be available at corresponding /dir URL)"`
}

const sdurl = "http://stablediff-cuda:7860"

var domains = map[string]echo.MiddlewareFunc{
	"":         proxy.NewProxyWrapperStr(sdurl, nil),
	"acestep.": proxy.NewProxyWrapperStr("http://acestep:7865", nil),
	"ovi.":     proxy.NewProxyWrapperStr("http://ovi:7860", nil),
}

var skipAuth = map[string][]string{
	"path": {
		"/login", "/metrics", "/internal/join", "/internal/leave", "/cui/join", "/cui/leave", "/cui/progress", "/acestep/join", "/acestep/leave", "/ovi/join", "/ovi/leave",
	},
	"prefix": {
		"/v1/", "/sdapi/",
	},
}

func post(path string) {
	_, err := http.Post(sdurl+path, "", nil)
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
	if params.AddUser {
		if params.Username == "" || params.Password == "" {
			log.Fatal("Specify username and password to add")
		}
		hashed, err := bcrypt.GenerateFromPassword([]byte(params.Password), bcrypt.DefaultCost)
		if err != nil {
			log.Fatalf("Error hashing password: %s", err)
		}
		err = loadCreds(params.CredFilename)
		if err != nil {
			log.Printf("Error loading existing users: %s, will create a new file and JWT secret", err)
			r := rand.New(rand.NewSource(time.Now().UnixNano()))
			params.JWTSecret = randomString(r, 64)
		}
		creds[strings.ToLower(params.Username)] = string(hashed)
		saveCreds(params.CredFilename)
		log.Printf("User %s added", params.Username)
		return
	}
	err = loadCreds(params.CredFilename)
	if err != nil {
		log.Fatal(err)
	}
	e := echo.New()
	mchan := metrics.NewMetrics(e, params.PushPassword)
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
	e.GET("/login", loginPageHandler)
	e.GET("/logout", logoutHandler)
	e.POST("/login", loginHandler)
	broker := events.NewBroker()
	wd := watchdog.NewWatchdog(params.FIFOPath)
	svcChan := make(chan servicequeue.SvcUpdate)
	sq := servicequeue.NewServiceQueue(svcChan)
	pr := progress.NewProgress(broker, params.SDHost, params.SDTimeout, wd, mchan, svcChan)
	pr.AddHandlers(e)
	pr.Start(sq)
	llmurl, err := url.Parse(params.LLMURL)
	if err != nil {
		log.Fatal(err)
	}
	e.Group("/*", earlyCheckMiddleware("/"), func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			for d, t := range domains {
				if c.Request().Host == d+params.Domain {
					return t(next)(c)
				}
			}
			return domains[""](next)(c)
		}
	})
	e.Group("/sdapi", domains[params.Domain])
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
	if params.LoRAPath != "" {
		upload.NewUploader(e.Group("/upload"), params.LoRAPath, params.CookieFile, broker, mchan)
	}
	if params.TTSURL != "" {
		ttsurl, err := url.Parse(params.TTSURL)
		if err != nil {
			log.Fatalf("Error parsing TTS URL: %s", err)
		}
		e.Group("/tts/*", earlyCheckMiddleware("/tts/"), middleware.Rewrite(map[string]string{"/tts/*": "/$1"}), newTTSProxy(ttsurl, sq, wd))
	}
	if params.CUIURL != "" {
		cuiurl, err := url.Parse(params.CUIURL)
		if err != nil {
			log.Fatalf("Error parsing CUI URL: %s", err)
		}
		addCUIHandlers(e, sq, cuiurl)
		e.Group("/cui/*", earlyCheckMiddleware("/cui/"), middleware.Rewrite(map[string]string{"/cui/*": "/$1"}), newCUIProxy(cuiurl))
	}
	if params.StaticPath != "" {
		dirs, err := os.ReadDir(params.StaticPath)
		if err != nil {
			log.Fatalf("Error reading static directory %s: %s", params.StaticPath, err)
		}
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			dirname := d.Name()
			e.Group("/"+dirname, earlyCheckMiddleware("/"+dirname+"/"), middleware.AddTrailingSlashWithConfig(middleware.TrailingSlashConfig{RedirectCode: 302, Skipper: func(c echo.Context) bool {
				return c.Path() != "/"+dirname
			}}), middleware.Static(filepath.Join(params.StaticPath, dirname)))
		}
	}
	e.Start(params.Address)
}
