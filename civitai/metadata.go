package civitai

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Downloader struct {
	c               *http.Client
	PreviewCopyPath string
}

func NewDownloader() (result *Downloader) {
	result = &Downloader{c: &http.Client{}}
	result.c.Timeout = time.Second * 30
	return result
}

func exists(filename string) bool {
	_, err := os.Stat(filename)
	return !os.IsNotExist(err)
}

func CopyLink(src string, dst string) error {
	err := os.Remove(dst)
	if err != nil {
		fmt.Printf("Removing %s failed: %s\n", dst, err)
	}
	err = os.Link(src, dst)
	if err == nil {
		return nil
	}
	fmt.Printf("Linking %s to %s failed: %s\n", src, dst, err)
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	tf, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer tf.Close()
	_, err = io.Copy(tf, sf)
	return err
}

func (d *Downloader) UpdateFile(filename string) error {
	return d.updateFile(filename, "")
}

// UpdateFileFromSource is like UpdateFile, but when the model is not found on
// CivitAI it records the given page URL (a Hugging Face file page, for remote
// uploads) in the "model page" field instead of an empty object.
func (d *Downloader) UpdateFileFromSource(filename, sourcePage string) error {
	return d.updateFile(filename, sourcePage)
}

func (d *Downloader) updateFile(filename string, sourcePage string) error {
	oldumask := maybeUmask(0111)
	defer maybeUmask(oldumask)
	ext := filepath.Ext(filename)
	if ext != ".safetensors" {
		return fmt.Errorf("invalid extension")
	}
	filebase := filename[:len(filename)-len(ext)]
	preview_img := filebase + ".preview.png"
	preview_vid := filebase + ".preview.mp4"
	jsonfilename := filebase + ".json"
	if exists(preview_img) || exists(preview_vid) || exists(jsonfilename) {
		return nil
	}
	modelfile, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer modelfile.Close()
	jsonfile, err := os.Create(jsonfilename)
	if err != nil {
		return err
	}
	defer jsonfile.Close()

	h := sha256.New()
	_, err = io.Copy(h, modelfile)
	if err != nil {
		return err
	}
	sum := h.Sum(nil)
	resp, err := d.c.Get(fmt.Sprintf("https://civitai.com/api/v1/model-versions/by-hash/%x", sum))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		if sourcePage != "" {
			var hfMetadata struct {
				ModelPage string `json:"model page"`
			}
			hfMetadata.ModelPage = sourcePage
			enc := json.NewEncoder(jsonfile)
			enc.SetIndent("", "    ")
			if err := enc.Encode(hfMetadata); err != nil {
				return err
			}
			return nil
		}
		jsonfile.WriteString("{}")
		return fmt.Errorf("model %s not found on CivitAI", filepath.Base(filename))
	}
	var civitaiMetadata struct {
		ID            int `json:"id"`
		ModelID       int `json:"modelId"`
		Description   string
		TrainedWords  []string
		BaseModel     string // "SD 1.5", "SDXL 1.0"
		Images        []struct {
			URL  string
			Type string
		}
		Error string
	}
	var metadata struct {
		Description    string `json:"description"`
		SDVersion      string `json:"sd version"`
		ActivationText string `json:"activation text"`
		ModelPage      string `json:"model page"`
	}
	json.NewDecoder(resp.Body).Decode(&civitaiMetadata)
	metadata.Description = civitaiMetadata.Description
	if strings.HasPrefix(civitaiMetadata.BaseModel, "SDXL") {
		metadata.SDVersion = "SDXL"
	} else if strings.HasPrefix(civitaiMetadata.BaseModel, "SD 1") {
		metadata.SDVersion = "SD1"
	} else if strings.HasPrefix(civitaiMetadata.BaseModel, "SD 2") {
		metadata.SDVersion = "SD2"
	} else {
		metadata.SDVersion = "unknown"
	}
	metadata.ActivationText = strings.Join(civitaiMetadata.TrainedWords, "; ")
	metadata.ModelPage = fmt.Sprintf("https://civitai.com/models/%d?modelVersionId=%d", civitaiMetadata.ModelID, civitaiMetadata.ID)

	enc := json.NewEncoder(jsonfile)
	enc.SetIndent("", "    ")
	err = enc.Encode(metadata)
	if err != nil {
		return err
	}

	for _, img := range civitaiMetadata.Images {
		if img.Type == "image" || img.Type == "video" {
			resp, err = d.c.Get(img.URL)
			if err != nil {
				return err
			}
			preview_name := preview_img
			if img.Type == "video" {
				preview_name = preview_vid
			}
			previewfile, err := os.Create(preview_name)
			if err != nil {
				return err
			}
			_, err = io.Copy(previewfile, resp.Body)
			if err != nil {
				previewfile.Close()
				return err
			}
			previewfile.Close()
			break
		}
	}
	return nil
}

// Walk walks root and applies perFile to every .safetensors file, reporting
// each (path, err) to the optional result callback.
func (d *Downloader) Walk(root string, perFile func(path string) error, result func(path string, err error)) error {
	return filepath.WalkDir(root, func(path string, dir fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("Error accessing %s: %s", path, err)
			return nil
		}
		if dir.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".safetensors" {
			err := perFile(path)
			if result != nil {
				result(path, err)
			}
		}
		return nil
	})
}

// UpdateModelPage fills the "model page" field of an existing model's JSON by
// looking the file up on CivitAI by hash. It leaves the file alone when there
// is no JSON, the field is already set, or the model is not on CivitAI.
func (d *Downloader) UpdateModelPage(filename string) error {
	ext := filepath.Ext(filename)
	if ext != ".safetensors" {
		return fmt.Errorf("invalid extension")
	}
	filebase := filename[:len(filename)-len(ext)]
	jsonfilename := filebase + ".json"
	if !exists(jsonfilename) {
		return nil
	}
	jsonfile, err := os.Open(jsonfilename)
	if err != nil {
		return err
	}
	defer jsonfile.Close()
	var metadata struct {
		Description    string `json:"description"`
		SDVersion      string `json:"sd version"`
		ActivationText string `json:"activation text"`
		ModelPage      string `json:"model page"`
	}
	if err := json.NewDecoder(jsonfile).Decode(&metadata); err != nil {
		return err
	}
	if metadata.ModelPage != "" {
		return nil
	}
	modelfile, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer modelfile.Close()
	h := sha256.New()
	if _, err := io.Copy(h, modelfile); err != nil {
		return err
	}
	sum := h.Sum(nil)
	resp, err := d.c.Get(fmt.Sprintf("https://civitai.com/api/v1/model-versions/by-hash/%x", sum))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil
	}
	var civitaiMetadata struct {
		ID      int `json:"id"`
		ModelID int `json:"modelId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&civitaiMetadata); err != nil {
		return err
	}
	metadata.ModelPage = fmt.Sprintf("https://civitai.com/models/%d?modelVersionId=%d", civitaiMetadata.ModelID, civitaiMetadata.ID)
	out, err := os.Create(jsonfilename)
	if err != nil {
		return err
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	enc.SetIndent("", "    ")
	return enc.Encode(metadata)
}
