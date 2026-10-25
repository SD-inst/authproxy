package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

//go:embed admin/webroot
var adminFS embed.FS

// adminLogin is the only account the container-control page and endpoint serve.
// The check is deliberately independent of the ACL: the ACL says which services
// a user may reach, while this is an operator capability. It is also independent
// of the JWT middleware, which is skipped on Caddy proxy-auth routes — the login
// is read from the sdkey cookie directly, so a request with no cookie of a
// different user never gets through.
const adminLogin = "admin"

// adminOnly allows the request only when the sdkey cookie belongs to adminLogin.
// Anyone else — including a valid session of another account — gets 403.
func adminOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !strings.EqualFold(authUser(c), adminLogin) {
			return echo.ErrForbidden
		}
		return next(c)
	}
}

// addAdminHandlers registers the operator page and the container control
// endpoints under /admin. Registering them as real routes (rather than leaving
// /admin to the catch-all domain proxy) matters twice over: the requests never
// reach ensureService, so opening the page cannot wake a container, and an
// unknown path under /admin answers 404 instead of being proxied to A1111.
func addAdminHandlers(e *echo.Echo, m *containerManager) {
	root, err := fs.Sub(adminFS, "admin/webroot")
	if err != nil {
		log.Fatal(err)
	}
	g := e.Group("/admin", adminOnly)
	g.GET("", func(c echo.Context) error {
		return c.Redirect(http.StatusPermanentRedirect, "/admin/")
	})
	g.GET("/", echo.StaticDirectoryHandler(root, false))
	g.GET("/containers", m.handleContainerStatus)
	g.POST("/container", m.handleContainerControl)
	// Explicit 404 for anything else under /admin. Without it an unknown path
	// falls through to the catch-all domain proxy, which would start A1111 for
	// a typo in the URL.
	g.Any("/*", func(c echo.Context) error {
		return echo.ErrNotFound
	})
}

// handleContainerStatus reports which containers the manager believes are
// running. This is the manager's own view — the one auto start/stop acts on —
// not a probe of Docker.
func (m *containerManager) handleContainerStatus(c echo.Context) error {
	return c.JSON(http.StatusOK, m.status())
}

// handleContainerControl runs one manual lifecycle op on one tracked container.
// The body names the service and the op; both are validated by
// containerManager.control against fixed sets, since they end up in the
// watchdog command line. The fresh status is returned so the page updates
// without a second request.
func (m *containerManager) handleContainerControl(c echo.Context) error {
	var p struct {
		Service string `json:"service"`
		Op      string `json:"op"`
	}
	if err := c.Bind(&p); err != nil {
		return JSONError(c, http.StatusBadRequest, err)
	}
	user := authUser(c)
	if err := m.control(p.Service, p.Op); err != nil {
		log.Printf("Container control %s %s by %s from %s failed: %s", p.Op, p.Service, user, c.RealIP(), err)
		return JSONErrorMessage(c, http.StatusBadGateway, err.Error())
	}
	log.Printf("Container control: user %s ran %s on %s from %s", user, p.Op, p.Service, c.RealIP())
	return c.JSON(http.StatusOK, m.status())
}
