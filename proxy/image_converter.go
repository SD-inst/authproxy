package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"golang.org/x/image/webp"
)

// setRequestBody sets the request body and updates headers
func setRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
}

// processImageURL processes an image URL and returns true if it was modified
func processImageURL(content map[string]any) bool {
	if content["type"] == "image_url" {
		if imageURL, ok := content["image_url"].(map[string]any); ok {
			if url, ok := imageURL["url"].(string); ok {
				if newURL, err := convertImageURL(url); err == nil {
					if newURL != url {
						imageURL["url"] = newURL
						return true
					}
				}
			}
		}
	}
	return false
}

// convertWebPToPNG converts WebP image data to PNG format
func convertWebPToPNG(webpData []byte) ([]byte, error) {
	img, err := webp.Decode(bytes.NewReader(webpData))
	if err != nil {
		return nil, fmt.Errorf("failed to decode WebP image: %w", err)
	}

	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)

	buf := new(bytes.Buffer)
	if err := png.Encode(buf, rgba); err != nil {
		return nil, fmt.Errorf("failed to encode PNG image: %w", err)
	}

	return buf.Bytes(), nil
}

// convertImageURL converts a single image URL from WebP to PNG
func convertImageURL(url string) (string, error) {
	if !strings.HasPrefix(url, "data:image/webp;") {
		return url, nil
	}

	parts := strings.Split(url, ",")
	if len(parts) != 2 {
		return url, nil
	}

	webpData, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return url, fmt.Errorf("failed to decode base64: %w", err)
	}

	pngData, err := convertWebPToPNG(webpData)
	if err != nil {
		return url, fmt.Errorf("failed to convert WebP to PNG: %w", err)
	}

	pngB64 := base64.StdEncoding.EncodeToString(pngData)
	return "data:image/png;base64," + pngB64, nil
}

// ConvertRequestIfNeeded converts WebP images to PNG in chat completions requests
func ConvertRequestIfNeeded(c echo.Context) error {
	if c.Request().Method != "POST" {
		return nil
	}

	path := c.Request().URL.Path
	if !strings.HasSuffix(path, "/v1/chat/completions") {
		return nil
	}

	bodyBytes, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err
	}

	c.Request().Body.Close()

	var requestData map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &requestData); err != nil {
		return err
	}

	if messagesJSON, ok := requestData["messages"]; ok {
		var messages []map[string]any
		if err := json.Unmarshal(messagesJSON, &messages); err != nil {
			return err
		}

		modified := false

		for _, msg := range messages {
			content := msg["content"]

			// Process map content (single image_url object)
			if contentMap, ok := content.(map[string]any); ok {
				if processImageURL(contentMap) {
					modified = true
				}
			}

			// Process array content (multi-modal messages)
			if contentArray, ok := content.([]any); ok {
				for _, item := range contentArray {
					if itemMap, ok := item.(map[string]any); ok {
						if processImageURL(itemMap) {
							modified = true
						}
					}
				}
			}
		}

		if modified {
			updatedMessages, err := json.Marshal(messages)
			if err != nil {
				return err
			}
			requestData["messages"] = updatedMessages
			updatedBody, err := json.Marshal(requestData)
			if err != nil {
				return err
			}
			setRequestBody(c.Request(), updatedBody)
		} else {
			setRequestBody(c.Request(), bodyBytes)
		}
	}

	return nil
}
