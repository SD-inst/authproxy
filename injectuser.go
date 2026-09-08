package main

import (
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

// headerAuthproxyUser is stamped onto service-bound requests so the service can
// attribute a task to the authenticated user (per-user task time limit).
const headerAuthproxyUser = "X-Authproxy-User"

// injectUser reads the sdkey JWT cookie and, if it is valid, stamps the
// authenticated username into the X-Authproxy-User header on the outgoing proxy
// request. A missing or invalid cookie leaves the request untouched, so the
// task is untracked (no per-user limit). It is used as a proxy Interceptor.Before
// hook on every SD_URL/ComfyUI proxy, so it works for both authenticated routes
// and the skipAuth /sdapi API route (where the browser still sends the cookie).
func injectUser(c echo.Context) {
	cookie, err := c.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	token, err := jwt.Parse(cookie.Value, func(t *jwt.Token) (interface{}, error) {
		return []byte(params.JWTSecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil || token == nil || !token.Valid {
		return
	}
	subject, err := token.Claims.GetSubject()
	if err != nil || subject == "" {
		return
	}
	c.Request().Header.Set(headerAuthproxyUser, subject)
}
