package upload

import (
	"github.com/dustin/go-humanize"
	"github.com/labstack/echo/v4"
	"golang.org/x/sys/unix"
)

func (u *uploader) stat(c echo.Context) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(u.root, &stat); err != nil {
		return JSONError(c, 500, err)
	}
	return JSONOk(c, Result{"free": humanize.IBytes(stat.Bavail * uint64(stat.Bsize))})
}
