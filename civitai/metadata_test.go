package civitai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type fakeRT struct {
	status   int
	body     string
	wantHash string // if set, the by-hash path must end with this
	called   *bool
}

func (f fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.called != nil {
		*f.called = true
	}
	if f.wantHash != "" && req.URL.Path != "/api/v1/model-versions/by-hash/"+f.wantHash {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(bytes.NewBufferString("{}")), Header: make(http.Header), Request: req}, nil
	}
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(bytes.NewBufferString(f.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func newTestDL(rt http.RoundTripper) *Downloader {
	d := NewDownloader()
	d.c = &http.Client{Transport: rt}
	return d
}

func writeSafetensors(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "model.safetensors")
	if err := os.WriteFile(p, []byte("dummy-model-bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readModelPage(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "model.json"))
	if err != nil {
		return ""
	}
	var m struct {
		ModelPage string `json:"model page"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	return m.ModelPage
}

func TestUpdateModelPage_FillsEmptyPage(t *testing.T) {
	dir := t.TempDir()
	sf := writeSafetensors(t, dir)
	// existing JSON with an empty model page
	if err := os.WriteFile(filepath.Join(dir, "model.json"), []byte(`{"description":"d","model page":""}`), 0644); err != nil {
		t.Fatal(err)
	}
	d := newTestDL(fakeRT{status: 200, body: `{"id":2514310,"modelId":827184,"baseModel":"SD 1.5"}`})
	if err := d.UpdateModelPage(sf); err != nil {
		t.Fatalf("UpdateModelPage() error = %v", err)
	}
	got := readModelPage(t, dir)
	want := "https://civitai.com/models/827184?modelVersionId=2514310"
	if got != want {
		t.Errorf("model page = %q, want %q", got, want)
	}
}

func TestUpdateModelPage_NotFoundOnCivitai(t *testing.T) {
	dir := t.TempDir()
	sf := writeSafetensors(t, dir)
	const before = `{"description":"d","model page":""}`
	if err := os.WriteFile(filepath.Join(dir, "model.json"), []byte(before), 0644); err != nil {
		t.Fatal(err)
	}
	d := newTestDL(fakeRT{status: 404, body: `{}`})
	if err := d.UpdateModelPage(sf); err != nil {
		t.Fatalf("UpdateModelPage() error = %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "model.json"))
	if string(data) != before {
		t.Errorf("JSON was modified, want it untouched: %s", string(data))
	}
}

func TestUpdateModelPage_AlreadySet(t *testing.T) {
	dir := t.TempDir()
	sf := writeSafetensors(t, dir)
	const existing = `{"model page":"https://huggingface.co/x/y/blob/main/z.gguf"}`
	if err := os.WriteFile(filepath.Join(dir, "model.json"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	var called bool
	d := newTestDL(fakeRT{status: 200, body: `{"id":1,"modelId":2}`, called: &called})
	if err := d.UpdateModelPage(sf); err != nil {
		t.Fatalf("UpdateModelPage() error = %v", err)
	}
	if called {
		t.Error("should not call CivitAI when the page is already set")
	}
	if p := readModelPage(t, dir); p != "https://huggingface.co/x/y/blob/main/z.gguf" {
		t.Errorf("model page changed to %q", p)
	}
}

func TestUpdateFile_HFFallbackOnNotFound(t *testing.T) {
	dir := t.TempDir()
	sf := writeSafetensors(t, dir)
	hfPage := "https://huggingface.co/empero-ai/Qwen3.8-27B-Ridge-GGUF/blob/main/Qwen3.8-27B-Ridge-3.7bpw.gguf"
	d := newTestDL(fakeRT{status: 404, body: `{}`})
	if err := d.UpdateFileFromSource(sf, hfPage); err != nil {
		t.Fatalf("UpdateFileFromSource() error = %v", err)
	}
	got := readModelPage(t, dir)
	if got != hfPage {
		t.Errorf("model page = %q, want the Hugging Face file page", got)
	}
}

func TestUpdateFile_NotFoundNoSource(t *testing.T) {
	dir := t.TempDir()
	sf := writeSafetensors(t, dir)
	d := newTestDL(fakeRT{status: 404, body: `{}`})
	if err := d.UpdateFile(sf); err == nil {
		t.Error("expected an error when not found on CivitAI and no source page")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "model.json"))
	if s := bytes.TrimSpace(data); string(s) != "{}" {
		t.Errorf("JSON = %q, want {} on not-found", string(data))
	}
}

func TestUpdateFile_CivitaiPagePopulated(t *testing.T) {
	dir := t.TempDir()
	sf := writeSafetensors(t, dir)
	d := newTestDL(fakeRT{status: 200, body: `{"id":2514310,"modelId":827184,"baseModel":"SDXL 1.0","trainedWords":["word"]}`})
	if err := d.UpdateFile(sf); err != nil {
		t.Fatalf("UpdateFile() error = %v", err)
	}
	got := readModelPage(t, dir)
	want := "https://civitai.com/models/827184?modelVersionId=2514310"
	if got != want {
		t.Errorf("model page = %q, want %q", got, want)
	}
}
