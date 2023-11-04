package upload

import (
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/labstack/echo/v4"
	"github.com/rkfg/authproxy/events"
	"golang.org/x/sys/unix"
)

//go:embed webroot
var webroot embed.FS

type dlTask struct {
	link string
	dir  string
}

type uploader struct {
	root       string
	broker     *events.Broker
	dlc        chan dlTask
	pageclient http.Client
	dlclient   http.Client
}

type downloadProgress struct {
	Filename       string `json:"filename"`
	TotalBytes     int64  `json:"total_bytes"`
	CompletedBytes int64  `json:"completed_bytes"`
}

type Result map[string]interface{}

type fileItem struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Timestamp int64  `json:"timestamp"`
}

var (
	validateRegexp = regexp.MustCompile(`[*<>]`)
	modelRegex     = regexp.MustCompile(`/models/(\d+)`)
)

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

func validateName(dir string) bool {
	return !validateRegexp.MatchString(dir)
}

func validateFilename(fn string) error {
	if !filepath.IsLocal(fn) {
		return fmt.Errorf("invalid file name")
	}
	if !validateName(fn) {
		return fmt.Errorf("invalid file name")
	}
	if !strings.HasSuffix(fn, ".safetensors") {
		return fmt.Errorf("only safetensors are supported")
	}
	return nil
}

func (u *uploader) postFiles(c echo.Context) error {
	dir := c.FormValue("dir")
	if !validateName(dir) {
		return JSONErrorMessage(c, 400, "Invalid directory name")
	}
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
		if err := validateFilename(file.Filename); err != nil {
			return JSONError(c, 400, err)
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
		name := f.Name()
		if f.IsDir() {
			t = "dir"
		} else {
			if !strings.HasSuffix(strings.ToLower(f.Name()), ".safetensors") {
				continue
			}
			name = strings.TrimSuffix(f.Name(), ".safetensors")
		}
		fi := fileItem{Type: t, Name: html.EscapeString(name)}
		if info, err := f.Info(); err != nil {
			log.Printf("Error getting file %s info: %s", f.Name(), err)
		} else {
			fi.Timestamp = info.ModTime().UnixMilli()
		}
		result = append(result, fi)
	}
	return JSONOk(c, result)
}

func (u *uploader) stat(c echo.Context) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(u.root, &stat); err != nil {
		return JSONError(c, 500, err)
	}
	return JSONOk(c, Result{"free": humanize.IBytes(stat.Bavail * uint64(stat.Bsize))})
}

func (u *uploader) download(c echo.Context) error {
	var params struct {
		URL string `form:"url"`
		Dir string `form:"dir"`
	}
	c.Bind(&params)
	if !validateName(params.Dir) {
		u.dlError("Invalid directory name: %s", params.Dir)
		return nil
	}
	cu, err := url.Parse(params.URL)
	if err != nil {
		u.dlError("Invalid URL: " + err.Error())
		return nil
	}
	if cu.Host != "civitai.com" {
		u.dlError("Only civitai.com is supported")
		return nil
	}
	m := modelRegex.FindStringSubmatch(cu.Path)
	if m == nil {
		u.dlError("Use the model page URL")
		return nil
	}
	mvid := cu.Query().Get("modelVersionId")
	if mvid != "" {
		resp, err := u.pageclient.Get("https://civitai.com/api/v1/model-versions/" + mvid)
		if err != nil {
			u.dlError("Error accessing CivitAI: %s", err)
			return nil
		}
		if resp.StatusCode != 200 {
			u.dlError("Error accessing CivitAI: code %d", resp.StatusCode)
		}
		var respModel struct {
			DownloadURL string `json:"downloadUrl"`
			Model       struct {
				Type string `json:"type"`
			}
		}
		json.NewDecoder(resp.Body).Decode(&respModel)
		if respModel.Model.Type != "LORA" && respModel.Model.Type != "LoCon" {
			u.dlError("Only LoRA download is supported, this is %s", respModel.Model.Type)
			return nil
		}
		u.dlc <- dlTask{link: respModel.DownloadURL, dir: params.Dir}
	} else {
		resp, err := u.pageclient.Get("https://civitai.com/api/v1/models/" + m[1])
		if err != nil {
			u.dlError("Error accessing CivitAI: %s", err)
			return nil
		}
		if resp.StatusCode != 200 {
			u.dlError("Error accessing CivitAI: code %d", resp.StatusCode)
		}
		var respModel struct {
			Type          string `json:"type"`
			ModelVersions []struct {
				DownloadURL string `json:"downloadUrl"`
			}
		}
		json.NewDecoder(resp.Body).Decode(&respModel)
		if respModel.Type != "LORA" && respModel.Type != "LoCon" {
			u.dlError("Only LoRA download is supported, this is %s", respModel.Type)
			return nil
		}
		if len(respModel.ModelVersions) == 0 {
			u.dlError("No model versions found.")
			return nil
		}
		u.dlc <- dlTask{link: respModel.ModelVersions[0].DownloadURL, dir: params.Dir}
	}
	return nil
}

func (u *uploader) dlMsg(msgType string, msg string, params ...any) {
	u.broker.Broadcast(events.Packet{Type: events.MESSAGE_UPDATE, Ephemeral: true, Data: events.MessageUpdate{Message: fmt.Sprintf(msg, params...), Type: msgType, Subsystem: "download"}})
}

func (u *uploader) dlError(msg string, params ...any) {
	u.dlMsg("error", msg, params...)
}

func (u *uploader) dlSuccess(msg string, params ...any) {
	u.dlMsg("success", msg, params...)
}

func (u *uploader) startDownloader() {
	for task := range u.dlc {
		func() {
			req, err := http.NewRequest("GET", task.link, nil)
			if err != nil {
				u.dlError("Error making request: %s", err)
				return
			}
			resp, err := u.dlclient.Do(req)
			if err != nil {
				u.dlError("Error downloading %s: %s", task.link, err)
				return
			}
			disp := resp.Header.Get(echo.HeaderContentDisposition)
			_, params, err := mime.ParseMediaType(disp)
			if err != nil {
				u.dlError("Error parsing disposition (check if you can download the file in incognito), disposition: '%s': %s", disp, err)
				return
			}
			fn := params["filename"]
			fullpath, err := u.fullPath(task.dir)
			if err != nil {
				u.dlError("Invalid directory %s: %s", task.dir, err)
				return
			}
			if err := validateFilename(fn); err != nil {
				u.dlError("Invalid filename %s: %s", fn, err)
				return
			}
			fullpath = filepath.Join(fullpath, fn)
			f, err := os.Create(fullpath)
			if err != nil {
				u.dlError("Error creating file %s: %s", fullpath, err)
				return
			}
			defer f.Close()
			total := resp.ContentLength
			dl := int64(0)
			log.Printf("Starting remote download: %s => %s", task.link, fullpath)
			for {
				n, err := io.CopyN(f, resp.Body, 1024*1024*10)
				dl += n
				if err != nil {
					u.broker.Broadcast(events.Packet{Type: events.DOWNLOAD_UPDATE, Data: downloadProgress{}})
					if err == io.EOF {
						u.dlSuccess("File %s downloaded", fn)
					} else {
						u.dlError("Error during download: %s", err)
						defer os.Remove(fullpath)
					}
					return
				}
				u.broker.Broadcast(events.Packet{Type: events.DOWNLOAD_UPDATE, Data: downloadProgress{Filename: fn, TotalBytes: total, CompletedBytes: dl}})
			}
		}()
	}
}

func NewUploader(api *echo.Group, rootPath string, broker *events.Broker) *uploader {
	os.MkdirAll(rootPath, 0755)
	result := uploader{root: rootPath, broker: broker, dlc: make(chan dlTask)}
	result.pageclient.Timeout = time.Second * 30
	api.StaticFS("*", echo.MustSubFS(webroot, "webroot"))
	api.GET("/files", result.listFiles)
	api.GET("/stat", result.stat)
	api.POST("/files", result.postFiles)
	api.POST("/download", result.download)
	go result.startDownloader()
	return &result
}
