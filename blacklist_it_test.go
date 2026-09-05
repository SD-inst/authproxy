package main

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// startBlacklistServer starts a real HTTP server (Echo) whose only middleware
// is the IP blacklist, and returns its listener. A real listener is required
// because the drop path hijacks the connection, which httptest's recorder does
// not support.
func startBlacklistServer(t *testing.T, b *ipBlacklist) net.Listener {
	t.Helper()
	e := echo.New()
	e.Use(ipBlacklistMiddleware(b))
	e.GET("/x", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: e}
	go func() {
		if err := srv.Serve(ln); err != nil {
			// srv.Close() returns ErrServerClosed; that's expected on cleanup.
			_ = err
		}
	}()
	t.Cleanup(func() { srv.Close() })
	return ln
}

// rawRequest sends a hand-built request and reads everything the server sends
// until the connection is closed.
func rawRequest(t *testing.T, ln net.Listener, xff string) string {
	t.Helper()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	req := "GET /x HTTP/1.1\r\nHost: t\r\nX-Forwarded-For: " + xff + "\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got []byte
	buf := make([]byte, 4096)
	for {
		n, rerr := conn.Read(buf)
		got = append(got, buf[:n]...)
		if rerr != nil {
			break // EOF or RST: connection torn down
		}
	}
	return string(got)
}

func TestBlacklistSeversConnection(t *testing.T) {
	b, err := buildIPBlacklist([]string{"203.0.113.0/24"})
	if err != nil {
		t.Fatalf("buildIPBlacklist = %v", err)
	}
	ln := startBlacklistServer(t, b)

	t.Run("blacklisted IP gets connection closed with no response", func(t *testing.T) {
		got := rawRequest(t, ln, "203.0.113.5")
		if strings.Contains(got, "HTTP/") {
			t.Fatalf("expected no HTTP status line for blacklisted IP, got: %q", got)
		}
	})

	t.Run("non-blacklisted IP gets a normal response", func(t *testing.T) {
		got := rawRequest(t, ln, "198.51.100.5")
		if !strings.Contains(got, "HTTP/1.1 200") {
			t.Fatalf("expected 200 for a non-blacklisted IP, got: %q", got)
		}
	})
}
