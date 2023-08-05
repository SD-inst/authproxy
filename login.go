package main

import (
	"bufio"
	"embed"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
)

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

const cookieName = "sdkey"

//go:embed templates/*
var templates embed.FS

var tpl = template.Must(template.ParseFS(templates, "templates/*"))

var creds = map[string]string{}

type Result map[string]interface{}

const (
	expirationDays = 7
	renewTime      = time.Hour * 24 * 6
)

func JSONError(c echo.Context, code int, err error) error {
	return JSONErrorMessage(c, code, err.Error())
}

// JSONErrorMessage returns a JSON error with the provided http error code and message
func JSONErrorMessage(c echo.Context, code int, msg string) error {
	return c.JSON(code, Result{"message": msg})
}

func loginPageHandler(c echo.Context) error {
	return tpl.Execute(c.Response(), struct{}{})
}

func failLogin(c echo.Context) error {
	return c.Redirect(302, "/login")
}

func setToken(c echo.Context, subject string) error {
	expiration := time.Now().AddDate(0, 0, expirationDays)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(expiration), Subject: subject})
	signed, err := token.SignedString([]byte(params.JWTSecret))
	if err != nil {
		return err
	}
	c.SetCookie(&http.Cookie{Name: cookieName, Value: signed, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expiration})
	return nil
}

func loginHandler(c echo.Context) error {
	login := strings.ToLower(c.FormValue("login"))
	password := c.FormValue("password")
	if login == "" {
		return failLogin(c)
	}
	passwordHashed, ok := creds[login]
	if !ok {
		return failLogin(c)
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHashed), []byte(password)) != nil {
		return failLogin(c)
	}
	err := setToken(c, login)
	if err != nil {
		return JSONError(c, 400, err)
	}
	return c.Redirect(302, "/")
}

func loadCreds(filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if params.JWTSecret == "" {
			params.JWTSecret = line
			continue
		}
		split := strings.Split(line, ":")
		if len(split) != 2 {
			log.Printf("invalid cred line: %s", line)
			continue
		}
		login := split[0]
		pwd := split[1]
		creds[login] = pwd
	}
	return nil
}

func saveCreds(filename string) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	f.WriteString(params.JWTSecret + "\n")
	for u, p := range creds {
		_, err := f.WriteString(fmt.Sprintf("%s:%s\n", u, p))
		if err != nil {
			return err
		}
	}
	return nil
}

func randomString(r *rand.Rand, length int) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = letterBytes[r.Int63()%int64(len(letterBytes))]
	}
	return string(b)
}

func keyErrorHandler(c echo.Context, err error) error {
	log.Print(err)
	return c.Redirect(302, "/login")
}

func earlyCheckMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().URL != nil && c.Request().URL.Path == "/" {
				token := c.Get("user").(*jwt.Token)
				if token != nil {
					date, err := token.Claims.GetExpirationTime()
					if err != nil {
						return JSONError(c, 400, err)
					}
					if time.Until(date.Time) < renewTime {
						subject, err := token.Claims.GetSubject()
						if err != nil {
							return JSONError(c, 400, err)
						}
						if subject == "" {
							return JSONErrorMessage(c, 400, "user not set")
						}
						if _, ok := creds[subject]; !ok {
							return JSONErrorMessage(c, 404, "user not found")
						}
						err = setToken(c, subject)
						if err != nil {
							return JSONError(c, 400, err)
						}
						log.Printf("Token renewed for user %s", subject)
					}
				}
			}
			return next(c)
		}
	}
}

func logoutHandler(c echo.Context) error {
	c.SetCookie(&http.Cookie{Name: cookieName, MaxAge: -1})
	c.Redirect(302, "/")
	return nil
}
