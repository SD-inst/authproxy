package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

// newCtx builds an echo context wrapping req/rec, the way the middleware chain
// does, so injectUser can read the cookie and stamp the header on the request.
func newCtx(t *testing.T, req *http.Request) echo.Context {
	t.Helper()
	e := echo.New()
	return e.NewContext(req, httptest.NewRecorder())
}

func TestInjectUserValid(t *testing.T) {
	oldSecret := params.JWTSecret
	params.JWTSecret = "testsecret"
	defer func() { params.JWTSecret = oldSecret }()

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "alice",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	signed, err := token.SignedString([]byte("testsecret"))
	if err != nil {
		t.Fatalf("sign token: %s", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: signed})
	injectUser(newCtx(t, req))
	if got := req.Header.Get(headerAuthproxyUser); got != "alice" {
		t.Fatalf("expected alice, got %q", got)
	}
}

func TestInjectUserNoCookie(t *testing.T) {
	oldSecret := params.JWTSecret
	params.JWTSecret = "testsecret"
	defer func() { params.JWTSecret = oldSecret }()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	injectUser(newCtx(t, req))
	if got := req.Header.Get(headerAuthproxyUser); got != "" {
		t.Fatalf("expected no header, got %q", got)
	}
}

func TestInjectUserInvalidCookie(t *testing.T) {
	oldSecret := params.JWTSecret
	params.JWTSecret = "testsecret"
	defer func() { params.JWTSecret = oldSecret }()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "not-a-jwt"})
	injectUser(newCtx(t, req))
	if got := req.Header.Get(headerAuthproxyUser); got != "" {
		t.Fatalf("expected no header for invalid cookie, got %q", got)
	}
}
