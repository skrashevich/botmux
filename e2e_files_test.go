package main

import (
	"bytes"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"testing"
)

// TestE2E_Files covers F-01..F-05: getFile forwarding, /api/media passthrough,
// WebP→PNG conversion, file-not-found, and the absence of a size limit.
func TestE2E_Files(t *testing.T) {
	h := setupE2E(t, withHTTPServer())

	token := "files:test1234567"
	botID := h.AddBot(BotConfig{
		Token:         token,
		Name:          "filesbot",
		BotUsername:   "filesbot",
		ManageEnabled: true,
	})

	// Load binary fixtures once for all subtests.
	jpegData, err := os.ReadFile(filepath.Join("testdata", "tg", "file_photo.jpg"))
	if err != nil {
		t.Fatalf("read file_photo.jpg: %v", err)
	}
	webpData, err := os.ReadFile(filepath.Join("testdata", "tg", "file_sticker.webp"))
	if err != nil {
		t.Fatalf("read file_sticker.webp: %v", err)
	}

	// F-01: getFile forwarding via /tgapi/bot{TOKEN}/getFile
	t.Run("F-01_getFile_forward", func(t *testing.T) {
		// Put the file so defaultGetFile can find it.
		h.fake.PutFile("photo.jpg", jpegData, "image/jpeg")

		status, resp := h.CallTgapi("getFile", token, map[string]any{"file_id": "photo.jpg"})
		if status != 200 {
			t.Fatalf("expected status 200, got %d; resp=%v", status, resp)
		}
		ok, _ := resp["ok"].(bool)
		if !ok {
			t.Fatalf("expected ok=true, got resp=%v", resp)
		}
		result, _ := resp["result"].(map[string]any)
		if result == nil {
			t.Fatalf("expected result map, got %v", resp["result"])
		}
		if result["file_id"] != "photo.jpg" {
			t.Errorf("file_id: got %v, want photo.jpg", result["file_id"])
		}
		if result["file_path"] != "photo.jpg" {
			t.Errorf("file_path: got %v, want photo.jpg", result["file_path"])
		}
	})

	// F-02: /api/media JPEG passthrough — bytes and Content-Type preserved.
	t.Run("F-02_media_jpeg_passthrough", func(t *testing.T) {
		h.fake.PutFile("photo.jpg", jpegData, "image/jpeg")

		resp, body := h.CallMedia(botID, "photo.jpg")
		if resp.StatusCode != 200 {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if ct != "image/jpeg" {
			t.Errorf("Content-Type: got %q, want image/jpeg", ct)
		}
		if !bytes.Equal(body, jpegData) {
			t.Errorf("body mismatch: got %d bytes, want %d bytes", len(body), len(jpegData))
		}
	})

	// F-03: /api/media WebP→PNG conversion — response is valid PNG.
	t.Run("F-03_media_webp_to_png", func(t *testing.T) {
		h.fake.PutFile("sticker.webp", webpData, "image/webp")

		resp, body := h.CallMedia(botID, "sticker.webp")
		if resp.StatusCode != 200 {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if ct != "image/png" {
			t.Errorf("Content-Type: got %q, want image/png", ct)
		}
		// Check PNG magic bytes: 89 50 4E 47 0D 0A 1A 0A
		pngMagic := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
		if len(body) < len(pngMagic) {
			t.Fatalf("body too short (%d bytes) to be a PNG", len(body))
		}
		if !bytes.Equal(body[:len(pngMagic)], pngMagic) {
			t.Errorf("PNG magic bytes mismatch: got %x, want %x", body[:len(pngMagic)], pngMagic)
		}
		// Also verify image.Decode accepts it as PNG.
		img, format, err := image.Decode(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("image.Decode failed: %v", err)
		}
		if format != "png" {
			t.Errorf("decoded format: got %q, want png", format)
		}
		if img.Bounds().Empty() {
			t.Error("decoded image has empty bounds")
		}
	})

	// F-04: /api/media file not found — fake returns 400, handleMediaProxy returns 500.
	t.Run("F-04_media_not_found", func(t *testing.T) {
		// "nonexistent" is not registered via PutFile so defaultGetFile returns 400.
		resp, _ := h.CallMedia(botID, "nonexistent")
		// handleMediaProxy: getFile decodes !ok → http.Error(w, "getFile failed", 500)
		if resp.StatusCode != 500 {
			t.Errorf("expected status 500 for missing file, got %d", resp.StatusCode)
		}
	})

	// F-05: no large-file size limit in handleMediaProxy — document the absence.
	t.Run("F-05_no_size_limit", func(t *testing.T) {
		// handleMediaProxy and proxyFileDownload stream directly via io.Copy with no
		// size cap. This subtest documents that behaviour by serving a 1 MiB payload
		// and verifying it is returned in full.
		largePNG := make([]byte, 1<<20) // 1 MiB of zeroes
		// Give it JPEG magic so Content-Type round-trips cleanly.
		copy(largePNG[:3], []byte{0xff, 0xd8, 0xff})
		h.fake.PutFile("large.jpg", largePNG, "image/jpeg")

		resp, body := h.CallMedia(botID, "large.jpg")
		if resp.StatusCode != 200 {
			t.Fatalf("expected status 200, got %d", resp.StatusCode)
		}
		if len(body) != len(largePNG) {
			t.Errorf("large file: got %d bytes, want %d bytes", len(body), len(largePNG))
		}
	})
}
