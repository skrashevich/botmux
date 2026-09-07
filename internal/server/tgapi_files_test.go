package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestValidFilePath(t *testing.T) {
	tests := []struct {
		path string
		ok   bool
	}{
		{"photos/file_1.jpg", true},
		{"/var/lib/telegram-bot-api/bot/documents/file.pdf", true},
		{"var/lib/telegram-bot-api/bot/documents/file.pdf", true},
		{"", false},
		{"../etc/passwd", false},
		{"photos/../secret", false},
		{"file://tmp/x", false},
	}
	for _, tc := range tests {
		if got := validFilePath(tc.path); got != tc.ok {
			t.Errorf("validFilePath(%q) = %v, want %v", tc.path, got, tc.ok)
		}
	}
}

func TestBuildUpstreamFileURL(t *testing.T) {
	got := buildUpstreamFileURL("http://localhost:8081", "123:token", "/var/lib/bot/photo.jpg")
	want := "http://localhost:8081/file/bot123:token/var/lib/bot/photo.jpg"
	if got != want {
		t.Fatalf("buildUpstreamFileURL: got %q, want %q", got, want)
	}
}

func TestRewriteGetFileResponse(t *testing.T) {
	in := []byte(`{"ok":true,"result":{"file_id":"abc","file_path":"/var/lib/telegram-bot-api/photo.jpg","file_size":42}}`)
	out := rewriteGetFileResponse(in)
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	result := resp["result"].(map[string]any)
	if result["file_path"] != "var/lib/telegram-bot-api/photo.jpg" {
		t.Fatalf("file_path: got %v", result["file_path"])
	}

	relative := []byte(`{"ok":true,"result":{"file_id":"abc","file_path":"photos/file.jpg"}}`)
	if got := string(rewriteGetFileResponse(relative)); string(got) != string(relative) {
		t.Fatalf("relative path should be unchanged: %s", got)
	}
}

func TestResolveLocalFilePath(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "documents", "file.txt")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{TgAPIFilesRoot: root}

	if _, ok := s.resolveLocalFilePath("documents/file.txt"); !ok {
		t.Fatal("expected relative path under root to resolve")
	}
	if _, ok := s.resolveLocalFilePath(filePath); !ok {
		t.Fatal("expected absolute path under root to resolve")
	}
	if _, ok := s.resolveLocalFilePath("/etc/passwd"); ok {
		t.Fatal("expected path outside root to be rejected")
	}

	sNoRoot := &Server{}
	if _, ok := sNoRoot.resolveLocalFilePath(filePath); !ok {
		t.Fatal("expected absolute path without root to resolve when file exists")
	}
	if _, ok := sNoRoot.resolveLocalFilePath("documents/file.txt"); ok {
		t.Fatal("expected relative path without root to fail")
	}
}
