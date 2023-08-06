package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"github.com/btcsuite/go-flags"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"golang.org/x/crypto/bcrypt"
)

var params struct {
	CredFilename string `short:"f" description:"Credentials filename" required:"true"`
	AddUser      bool   `short:"a" description:"Add new user"`
	Username     string `short:"u" description:"Username for -a"`
	Password     string `short:"p" description:"Password for -a"`
	TargetURL    string `short:"t" description:"Target URL to proxy to"`
	LLMURL       string `long:"llm-url" description:"Target LLM URL to proxy to"`
	LLMStreamURL string `long:"llm-stream-url" description:"Target LLM stream (websocket) URL to proxy to"`
	Address      string `short:"l" description:"Listen at this address" default:"0.0.0.0:8000"`
	LLMTimeout   int    `long:"llm-timeout" description:"Number of minutes after which the LLM will be automatically unloaded to free VRAM" default:"10"`
	LLMModel     string `long:"llm-model" description:"LLM model to autoload"`
	LLMArgs      string `long:"llm-args" description:"JSON-formatted parameters to load the model and loras"`
	JWTSecret    string
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
		creds[params.Username] = string(hashed)
		saveCreds(params.CredFilename)
		log.Printf("User %s added", params.Username)
		return
	}
	if params.TargetURL == "" {
		log.Fatal("Specify the target URL to proxy requests to (-t http://127.0.0.1...)")
	}
	err = loadCreds(params.CredFilename)
	if err != nil {
		log.Fatal(err)
	}
	e := echo.New()
	e.Use(echojwt.WithConfig(echojwt.Config{
		SigningKey:   []byte(params.JWTSecret),
		ErrorHandler: keyErrorHandler,
		TokenLookup:  "cookie:" + cookieName,
		Skipper: func(c echo.Context) bool {
			return c.Path() == "/login" || strings.HasPrefix(c.Path(), "/api/")
		},
	}))
	e.GET("/login", loginPageHandler)
	e.GET("/logout", logoutHandler)
	e.POST("/login", loginHandler)
	tgturl, err := url.Parse(params.TargetURL)
	if err != nil {
		log.Fatal(err)
	}
	e.Group("/*", earlyCheckMiddleware(), middleware.Proxy(middleware.NewRoundRobinBalancer([]*middleware.ProxyTarget{
		{URL: tgturl},
	})))

	llmurl, err := url.Parse(params.LLMURL)
	if err != nil {
		log.Fatal(err)
	}
	if llmurl != nil {
		if params.LLMModel == "" {
			log.Fatal("Specify the LLM model name")
		}
		llmstreamurl, err := url.Parse(params.LLMStreamURL)
		if err != nil {
			log.Fatal(err)
		}
		llm := NewLLMBalancer(llmurl, llmstreamurl)
		llm.timeoutMins = params.LLMTimeout
		llm.modelName = params.LLMModel
		json.NewDecoder(strings.NewReader(params.LLMArgs)).Decode(&llm.args)
		llm.updateTimeout()
		e.Group("/api/*", middleware.ProxyWithConfig(middleware.ProxyConfig{
			Balancer: llm,
		}))
		e.GET("/api/v1/model", llm.handleModel)
	}
	e.Start(params.Address)
}
