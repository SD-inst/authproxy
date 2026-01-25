package upload

import (
	"github.com/labstack/echo/v4"
)

func (u *uploader) stat(c echo.Context) error {
	return JSONOk(c, Result{"free": "unsupported"})
}
