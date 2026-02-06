package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

type ACLElement struct {
	Domain string
	Path   string
}

var (
	whitelist      = map[string]map[string][]string{} // login => domain => path => struct{}
	blacklist      = map[string]map[string][]string{}
	fullaccess     = map[string]struct{}{}
	serviceMapping = map[string]ACLElement{
		"a1111":     {Domain: "", Path: "/"},
		"status":    {Domain: "", Path: "/q"},
		"comfyui":   {Domain: "", Path: "/cui"},
		"cozyui":    {Domain: "", Path: "/cozyui"},
		"tts":       {Domain: "", Path: "/tts"},
		"acestep":   {Domain: "acestep", Path: "/"},
		"acestep15": {Domain: "as15", Path: "/"},
		"ovi":       {Domain: "ovi", Path: "/"},
		"llm":       {Domain: "", Path: "/upstream"},
	}
)

func putACL(login string, service string) error {
	list := whitelist
	if strings.HasPrefix(service, "-") {
		list = blacklist
		service = strings.TrimPrefix(service, "-")
	}
	e, ok := serviceMapping[service]
	if !ok {
		return fmt.Errorf("unknown service %s for login %s", service, login)
	}
	if _, ok := list[login]; !ok {
		list[login] = map[string][]string{}
	}
	domain := e.Domain
	if domain != "" {
		domain += "."
	}
	list[login][domain] = append(list[login][domain], e.Path)
	return nil
}

func checkACL(domain string, path string, login string) bool {
	if len(whitelist) == 0 && len(blacklist) == 0 { // no acl loaded, allow all
		return true
	}
	if _, ok := fullaccess[login]; ok {
		return true
	}
	if _, ok := whitelist[login]; !ok {
		return false
	}
	if _, ok := whitelist[login][domain]; !ok {
		return false
	}
	allowed := false
	for _, p := range whitelist[login][domain] {
		if strings.HasPrefix(path, p) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	if _, ok := blacklist[login]; !ok {
		return true
	}
	if _, ok := blacklist[login][domain]; !ok {
		return true
	}
	for _, p := range blacklist[login][domain] {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	return true
}

func loadACL() error {
	for login, services := range config.ACL {
		if len(services) == 1 && services[0] == "*" {
			fullaccess[login] = struct{}{}
		} else {
			for _, s := range services {
				if err := putACL(login, s); err != nil {
					log.Fatal(err)
				}
			}
		}
	}
	return nil
}

func aclMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			token := c.Get("user")
			if token != nil {
				claims := token.(*jwt.Token).Claims
				if claims != nil {
					subject, err := claims.GetSubject()
					if err == nil && subject != "" {
						domain := strings.TrimSuffix(c.Request().Host, config.Domain)
						path := c.Request().URL.Path
						if !checkACL(domain, path, subject) {
							log.Printf("ACL access denied for user %s to %s %s", subject, domain, path)
							return echo.ErrForbidden
						}
					}
				}
			}
			return next(c)
		}
	}
}
