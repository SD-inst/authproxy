package proxy

import (
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

type Interceptor struct {
	Before func(c echo.Context)
	After  func(req *http.Request, resp *http.Response) error
}

type proxyWrapper struct {
	middleware.ProxyBalancer
	i *Interceptor
}

func (pw *proxyWrapper) Next(c echo.Context) *middleware.ProxyTarget {
	if pw.i != nil && pw.i.Before != nil {
		pw.i.Before(c)
	}
	return pw.ProxyBalancer.Next(c)
}

func NewProxyWrapper(targetURL *url.URL, i *Interceptor) echo.MiddlewareFunc {
	return middleware.ProxyWithConfig(middleware.ProxyConfig{
		Balancer: &proxyWrapper{ProxyBalancer: middleware.NewRoundRobinBalancer([]*middleware.ProxyTarget{
			{URL: targetURL},
		}), i: i},
		ErrorHandler: func(c echo.Context, err error) error {
			if i != nil && i.After != nil {
				if err := i.After(c.Request(), nil); err != nil {
					return err
				}
			}
			return err
		},
		ModifyResponse: func(r *http.Response) error {
			if i != nil && i.After != nil {
				return i.After(r.Request, r)
			}
			return nil
		},
	})
}
