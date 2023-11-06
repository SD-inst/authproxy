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
	c *http.Client
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

func (d *Downloader) UpdateFile(filename string) error {
	ext := filepath.Ext(filename)
	if ext != ".safetensors" {
		return fmt.Errorf("invalid extension")
	}
	filebase := filename[:len(filename)-len(ext)]
	preview := filebase + ".preview.png"
	if exists(preview) {
		return nil
	}
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	if err != nil {
		return err
	}
	sum := h.Sum(nil)
	resp, err := d.c.Get(fmt.Sprintf("https://civitai.com/api/v1/model-versions/by-hash/%x", sum))
	if err != nil {
		return err
	}
	if resp.StatusCode == 404 {
		return fmt.Errorf("model %s not found on CivitAI", filepath.Base(filename))
	}
	var civitaiMetadata struct {
		Description  string
		TrainedWords []string
		BaseModel    string // "SD 1.5", "SDXL 1.0"
		Images       []struct {
			URL  string
			Type string
		}
		Error string
	}
	var metadata struct {
		Description    string `json:"description"`
		SDVersion      string `json:"sd version"`
		ActivationText string `json:"activation text"`
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

	f, err = os.Create(filebase + ".json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "    ")
	err = enc.Encode(metadata)
	if err != nil {
		return err
	}

	for _, img := range civitaiMetadata.Images {
		if img.Type == "image" {
			resp, err = d.c.Get(img.URL)
			if err != nil {
				return err
			}
			f, err = os.Create(preview)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(f, resp.Body)
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func (d *Downloader) Walk(root string, result func(path string, err error)) error {
	return filepath.WalkDir(root, func(path string, dir fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("Error accessing %s: %s", path, err)
			return nil
		}
		if dir.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".safetensors" {
			err := d.UpdateFile(path)
			if result != nil {
				result(path, err)
			}
		}
		return nil
	})
}
