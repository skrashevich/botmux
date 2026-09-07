package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	webp "github.com/skrashevich/go-webp"
)

const (
	tgapiMaxBodyCapture = 50 * 1024 * 1024
	tgapiMaxBodyLocal   = 2100 * 1024 * 1024
	tgapiMaxBodyCloud   = 50 * 1024 * 1024
)

// validFilePath whitelists Telegram file paths. Cloud API returns relative paths
// like "photos/file_N.jpg"; Local Bot API --local mode returns absolute paths.
func validFilePath(p string) bool {
	if p == "" || len(p) > 4096 {
		return false
	}
	if strings.Contains(p, "..") {
		return false
	}
	if strings.ContainsAny(p, ":#?@\\") {
		return false
	}
	return true
}

func (s *Server) isCustomTgAPI() bool {
	base := s.tgAPIURL()
	return base != "" && base != "https://api.telegram.org"
}

func (s *Server) tgapiMaxBodySize() int64 {
	if s.isCustomTgAPI() {
		return tgapiMaxBodyLocal
	}
	return tgapiMaxBodyCloud
}

func (s *Server) tgapiProxyTimeout() time.Duration {
	if s.isCustomTgAPI() {
		return 10 * time.Minute
	}
	return 30 * time.Second
}

func (s *Server) tgapiDownloadTimeout() time.Duration {
	if s.isCustomTgAPI() {
		return 10 * time.Minute
	}
	return 60 * time.Second
}

func (s *Server) tgapiGetFileTimeout() time.Duration {
	if s.isCustomTgAPI() {
		return 2 * time.Minute
	}
	return 15 * time.Second
}

// buildUpstreamFileURL builds the HTTP download URL for a Telegram file path.
func buildUpstreamFileURL(base, token, filePath string) string {
	p := strings.TrimPrefix(filePath, "/")
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return fmt.Sprintf("%s/file/bot%s/%s", strings.TrimRight(base, "/"), token, strings.Join(segments, "/"))
}

// rewriteGetFileResponse rewrites absolute file_path values from Local Bot API so
// backends can download via BotMux /tgapi/file/bot{TOKEN}/...
func rewriteGetFileResponse(respBody []byte) []byte {
	var resp map[string]any
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return respBody
	}
	ok, _ := resp["ok"].(bool)
	if !ok {
		return respBody
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return respBody
	}
	fp, ok := result["file_path"].(string)
	if !ok || !strings.HasPrefix(fp, "/") {
		return respBody
	}
	result["file_path"] = strings.TrimPrefix(fp, "/")
	out, err := json.Marshal(resp)
	if err != nil {
		return respBody
	}
	return out
}

// resolveLocalFilePath returns a readable local filesystem path when BotMux can
// serve the file directly (colocated Local Bot API or shared volume).
func (s *Server) resolveLocalFilePath(filePath string) (string, bool) {
	if filePath == "" {
		return "", false
	}

	candidate := filePath
	if !filepath.IsAbs(filePath) {
		if s.TgAPIFilesRoot == "" {
			return "", false
		}
		candidate = filepath.Join(s.TgAPIFilesRoot, filePath)
	}

	candidate = filepath.Clean(candidate)
	if s.TgAPIFilesRoot != "" {
		root := filepath.Clean(s.TgAPIFilesRoot)
		rel, err := filepath.Rel(root, candidate)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "", false
		}
	} else if !filepath.IsAbs(filePath) {
		return "", false
	}

	st, err := os.Stat(candidate)
	if err != nil || st.IsDir() {
		return "", false
	}
	return candidate, true
}

func (s *Server) serveTelegramFile(w http.ResponseWriter, r *http.Request, token, filePath string, fileSize int64) {
	if localPath, ok := s.resolveLocalFilePath(filePath); ok {
		serveLocalFile(w, r, localPath, filePath, fileSize)
		return
	}
	downloadURL := buildUpstreamFileURL(s.tgAPIURL(), token, filePath)
	proxyFileDownload(w, r, downloadURL, filePath, fileSize, s.tgapiDownloadTimeout())
}

func serveLocalFile(w http.ResponseWriter, r *http.Request, localPath, displayPath string, fileSize int64) {
	if strings.HasSuffix(strings.ToLower(displayPath), ".webp") {
		body, err := os.ReadFile(localPath)
		if err != nil {
			http.Error(w, "read failed", 500)
			return
		}
		img, err := webp.Decode(bytes.NewReader(body))
		if err != nil {
			w.Header().Set("Content-Type", "image/webp")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(body)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		png.Encode(w, img)
		return
	}

	f, err := os.Open(localPath)
	if err != nil {
		http.Error(w, "open failed", 500)
		return
	}
	defer f.Close()

	ct := mime.TypeByExtension(path.Ext(localPath))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if fileSize > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileSize))
	}
	http.ServeContent(w, r, filepath.Base(localPath), time.Time{}, f)
}
