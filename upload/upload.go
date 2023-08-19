package upload

import (
	"embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"
)

//go:embed webroot
var webroot embed.FS

type uploader struct {
	root string
}

type Result map[string]interface{}

type fileItem struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func JSONOk(c echo.Context, r interface{}) error {
	return c.JSON(http.StatusOK, r)
}

func JSONError(c echo.Context, code int, err error) error {
	return JSONErrorMessage(c, code, err.Error())
}

func JSONErrorMessage(c echo.Context, code int, msg string) error {
	return c.JSON(code, Result{"message": msg})
}

func (u *uploader) fullPath(dir string) (string, error) {
	if dir != "" && !filepath.IsLocal(dir) {
		return "", fmt.Errorf("invalid path")
	}
	return filepath.Join(u.root, dir), nil
}

func (u *uploader) postFiles(c echo.Context) error {
	dir := c.FormValue("dir")
	typ := c.FormValue("type")
	fullpath, err := u.fullPath(dir)
	if err != nil {
		return JSONError(c, 400, err)
	}
	switch typ {
	case "create_dir":
		if err := os.MkdirAll(fullpath, 0755); err != nil {
			return JSONError(c, 500, err)
		}
		return nil
	case "upload_file":
		file, err := c.FormFile("file")
		if err != nil {
			return JSONError(c, 400, err)
		}
		if file.Size > 1024*1024*1024 { // 1 Gb
			return JSONErrorMessage(c, 400, "file too big")
		}
		if !filepath.IsLocal(file.Filename) {
			return JSONErrorMessage(c, 400, "invalid file name")
		}
		source, err := file.Open()
		if err != nil {
			return JSONError(c, 400, err)
		}
		defer source.Close()
		target, err := os.Create(filepath.Join(fullpath, file.Filename))
		if err != nil {
			return JSONError(c, 400, err)
		}
		defer target.Close()
		_, err = io.Copy(target, source)
		if err != nil {
			return JSONError(c, 400, err)
		}
	}
	return nil
}

func (u *uploader) listFiles(c echo.Context) error {
	dir := c.QueryParam("dir")
	fullpath, err := u.fullPath(dir)
	if err != nil {
		return JSONError(c, 400, err)
	}
	files, err := os.ReadDir(fullpath)
	if err != nil {
		return JSONError(c, 500, err)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir() && !files[j].IsDir() {
			return true
		}
		if !files[i].IsDir() && files[j].IsDir() {
			return false
		}
		return strings.Compare(strings.ToLower(files[i].Name()), strings.ToLower(files[j].Name())) < 0
	})
	result := []fileItem{}
	for _, f := range files {
		t := "file"
		if f.IsDir() {
			t = "dir"
		}
		result = append(result, fileItem{Type: t, Name: f.Name()})
	}
	return JSONOk(c, result)
}

func NewUploader(api *echo.Group, rootPath string) *uploader {
	os.MkdirAll(rootPath, 0755)
	result := uploader{root: rootPath}
	api.StaticFS("*", echo.MustSubFS(webroot, "webroot"))
	api.GET("/files", result.listFiles)
	api.POST("/files", result.postFiles)
	return &result
}
