package huggingface

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const maxFileSize = 1024 * 1024 * 1024 // 1 GB

var hfURLRegex = regexp.MustCompile(`^https?://(?:www\.)?huggingface\.co/([^/]+)/([^/]+)/(?:blob|resolve)/(.+)/(.+)$`)

type hfURL struct {
	user   string
	repo   string
	branch string
	file   string
}

func parseHFURL(rawURL string) (*hfURL, error) {
	m := hfURLRegex.FindStringSubmatch(rawURL)
	if m == nil {
		return nil, fmt.Errorf("not a valid Hugging Face URL")
	}
	return &hfURL{
		user:   m[1],
		repo:   m[2],
		branch: m[3],
		file:   m[4],
	}, nil
}

func (u *hfURL) resolveURL() string {
	file := u.file
	if idx := strings.Index(file, "?"); idx != -1 {
		file = file[:idx]
	}
	return fmt.Sprintf("https://huggingface.co/%s/%s/resolve/%s/%s?download=true", u.user, u.repo, u.branch, url.PathEscape(file))
}

type Downloader struct {
	client *http.Client
}

func NewDownloader() *Downloader {
	return &Downloader{
		client: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

func (d *Downloader) GetDownloadURL(rawURL string) (string, int64, error) {
	hf, err := parseHFURL(rawURL)
	if err != nil {
		return "", 0, err
	}
	downloadURL := hf.resolveURL()

	req, err := http.NewRequest("HEAD", downloadURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("error creating request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("error checking file size: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("Hugging Face returned status %d for %s", resp.StatusCode, downloadURL)
	}

	sizeStr := resp.Header.Get("Content-Length")
	if sizeStr == "" {
		return "", 0, fmt.Errorf("no Content-Length header from Hugging Face")
	}
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("invalid Content-Length: %w", err)
	}

	if size > maxFileSize {
		return "", 0, fmt.Errorf("file too large: %.2f GB (max 1 GB)", float64(size)/(1024*1024*1024))
	}

	return downloadURL, size, nil
}
