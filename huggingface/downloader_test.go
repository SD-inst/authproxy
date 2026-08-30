package huggingface

import "testing"

func TestFilePageURL(t *testing.T) {
	d := NewDownloader()
	tests := []struct {
		in   string
		want string
	}{
		// resolve (direct) form is normalized to the blob (file page) form
		{
			in:   "https://huggingface.co/empero-ai/Qwen3.8-27B-Ridge-GGUF/resolve/main/Qwen3.8-27B-Ridge-3.7bpw.gguf",
			want: "https://huggingface.co/empero-ai/Qwen3.8-27B-Ridge-GGUF/blob/main/Qwen3.8-27B-Ridge-3.7bpw.gguf",
		},
		// blob form is kept as-is
		{
			in:   "https://huggingface.co/empero-ai/Qwen3.8-27B-Ridge-GGUF/blob/main/Qwen3.8-27B-Ridge-3.7bpw.gguf",
			want: "https://huggingface.co/empero-ai/Qwen3.8-27B-Ridge-GGUF/blob/main/Qwen3.8-27B-Ridge-3.7bpw.gguf",
		},
		// www. prefix and query string are normalized away
		{
			in:   "https://www.huggingface.co/user/repo/resolve/branch/nested/model.safetensors?download=true",
			want: "https://huggingface.co/user/repo/blob/branch/nested/model.safetensors",
		},
	}
	for _, tt := range tests {
		got, err := d.FilePageURL(tt.in)
		if err != nil {
			t.Errorf("FilePageURL(%q) error = %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("FilePageURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFilePageURLInvalid(t *testing.T) {
	d := NewDownloader()
	for _, in := range []string{
		"https://civitai.com/models/123",
		"https://huggingface.co/user/repo", // missing blob/resolve/file
	} {
		if _, err := d.FilePageURL(in); err == nil {
			t.Errorf("FilePageURL(%q) expected an error", in)
		}
	}
}
