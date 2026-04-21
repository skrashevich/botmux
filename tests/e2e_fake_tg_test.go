package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordedRequest struct {
	timestamp time.Time
	method    string // e.g. "sendMessage", "getUpdates"
	token     string // extracted from URL path
	body      []byte // raw request body (may be JSON or multipart)
	query     url.Values
	headers   http.Header
}

type tgBotMeta struct {
	id        int64
	username  string
	firstName string
}

type fakeTG struct {
	t            *testing.T
	mu           sync.Mutex
	server       *httptest.Server
	requests     []recordedRequest
	handlers     map[string]http.HandlerFunc // method override
	botInfo      map[string]tgBotMeta        // token -> meta
	updates      map[string][]map[string]any // token -> FIFO queue
	offsetCursor map[string]int              // token -> last consumed update_id
	files        map[string][]byte           // file_path -> bytes
	contentTypes map[string]string           // file_path -> Content-Type
	msgIDCounter int64                       // monotonic message IDs
}

func newFakeTG(t *testing.T) *fakeTG {
	t.Helper()
	f := &fakeTG{
		t:            t,
		handlers:     make(map[string]http.HandlerFunc),
		botInfo:      make(map[string]tgBotMeta),
		updates:      make(map[string][]map[string]any),
		offsetCursor: make(map[string]int),
		files:        make(map[string][]byte),
		contentTypes: make(map[string]string),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.server.Close)
	return f
}

// RegisterBot registers a bot token with the fake server.
func (f *fakeTG) RegisterBot(token string, username string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.botInfo[token] = tgBotMeta{id: id, username: username, firstName: username}
}

// EnqueueUpdate adds an update to the queue for a bot token.
func (f *fakeTG) EnqueueUpdate(token string, update map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates[token] = append(f.updates[token], update)
}

// PutFile stores a file under path for later serving.
func (f *fakeTG) PutFile(path string, data []byte, contentType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = data
	f.contentTypes[path] = contentType
}

// SetHandler overrides the default handler for a Telegram method.
func (f *fakeTG) SetHandler(method string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = h
}

// Requests returns a snapshot of all recorded requests.
func (f *fakeTG) Requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// RequestsFor returns all recorded requests matching a method name.
func (f *fakeTG) RequestsFor(method string) []recordedRequest {
	var out []recordedRequest
	for _, r := range f.Requests() {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

// RequestsCountFor returns the number of recorded requests for a method.
func (f *fakeTG) RequestsCountFor(method string) int {
	return len(f.RequestsFor(method))
}

// URL returns the base URL of the fake server.
func (f *fakeTG) URL() string { return f.server.URL }

// Close shuts down the fake server.
func (f *fakeTG) Close() { f.server.Close() }

// writeJSON writes a JSON response with the given status code.
func (f *fakeTG) writeJSON(w http.ResponseWriter, code int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		f.t.Errorf("fakeTG: writeJSON encode error: %v", err)
	}
}

// route is the main HTTP handler for the fake Telegram server.
func (f *fakeTG) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Handle file downloads: GET /file/bot{TOKEN}/{path}
	if strings.HasPrefix(path, "/file/bot") {
		rest := strings.TrimPrefix(path, "/file/bot")
		// rest = TOKEN/file_path
		slashIdx := strings.Index(rest, "/")
		if slashIdx < 0 {
			http.NotFound(w, r)
			return
		}
		filePath := rest[slashIdx+1:]
		f.mu.Lock()
		data, ok := f.files[filePath]
		ct := f.contentTypes[filePath]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ct)
		_, _ = w.Write(data)
		return
	}

	// Handle API calls: /bot{TOKEN}/{method}
	if !strings.HasPrefix(path, "/bot") {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(path, "/bot")
	slashIdx := strings.Index(rest, "/")
	if slashIdx < 0 {
		http.NotFound(w, r)
		return
	}
	token := rest[:slashIdx]
	method := rest[slashIdx+1:]

	// Read body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("fakeTG: read body error: %v", err)
	}

	rec := recordedRequest{
		timestamp: time.Now(),
		method:    method,
		token:     token,
		body:      bodyBytes,
		query:     r.URL.Query(),
		headers:   r.Header.Clone(),
	}

	f.mu.Lock()
	f.requests = append(f.requests, rec)
	customHandler, hasCustom := f.handlers[method]
	f.mu.Unlock()

	// Restore body for handlers that may re-read it
	r.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))

	if hasCustom {
		customHandler(w, r)
		return
	}

	f.handleDefault(w, r, token, method, bodyBytes)
}

// handleDefault dispatches to the built-in default handlers.
func (f *fakeTG) handleDefault(w http.ResponseWriter, r *http.Request, token, method string, bodyBytes []byte) {
	switch method {
	case "getMe":
		code, body := f.defaultGetMe(token)
		f.writeJSON(w, code, body)

	case "getUpdates":
		f.defaultGetUpdates(w, r, token, bodyBytes)

	case "sendMessage", "sendPhoto", "sendVideo", "sendAudio", "sendDocument",
		"sendAnimation", "sendVoice", "sendVideoNote", "sendSticker":
		f.defaultSend(w, r, token, method, bodyBytes)

	case "copyMessage":
		f.defaultCopyMessage(w, r, token, bodyBytes)

	case "forwardMessage":
		f.defaultForwardMessage(w, r, token, bodyBytes)

	case "editMessageText":
		f.defaultEditMessageText(w, r, token, bodyBytes)

	case "deleteMessage":
		f.writeJSON(w, 200, map[string]any{"ok": true, "result": true})

	case "getFile":
		f.defaultGetFile(w, r, token, bodyBytes)

	default:
		f.writeJSON(w, 200, map[string]any{"ok": true, "result": true})
	}
}

func (f *fakeTG) defaultGetMe(token string) (int, map[string]any) {
	f.mu.Lock()
	meta, ok := f.botInfo[token]
	f.mu.Unlock()
	if !ok {
		return 401, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"}
	}
	return 200, map[string]any{
		"ok": true,
		"result": map[string]any{
			"id":                          meta.id,
			"username":                    meta.username,
			"first_name":                  meta.firstName,
			"is_bot":                      true,
			"can_join_groups":             true,
			"can_read_all_group_messages": false,
			"supports_inline_queries":     false,
		},
	}
}

// parseIntParam extracts an integer parameter from query string or JSON/form body.
func parseIntParam(r *http.Request, bodyBytes []byte, key string) int {
	// Try query string first
	if v := r.URL.Query().Get(key); v != "" {
		var n int
		if _, err := parseSimpleInt(v, &n); err == nil {
			return n
		}
	}

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		var m map[string]any
		if json.Unmarshal(bodyBytes, &m) == nil {
			if v, ok := m[key]; ok {
				switch val := v.(type) {
				case float64:
					return int(val)
				case json.Number:
					n, _ := val.Int64()
					return int(n)
				}
			}
		}
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"),
		strings.HasPrefix(ct, "multipart/form-data"):
		_ = r.ParseForm()
		if v := r.FormValue(key); v != "" {
			var n int
			if _, err := parseSimpleInt(v, &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// parseSimpleInt parses a decimal string into an int pointer.
func parseSimpleInt(s string, out *int) (int, error) {
	n := 0
	neg := false
	for i, ch := range s {
		if i == 0 && ch == '-' {
			neg = true
			continue
		}
		if ch < '0' || ch > '9' {
			return 0, &url.Error{Op: "parse", URL: s}
		}
		n = n*10 + int(ch-'0')
	}
	if neg {
		n = -n
	}
	*out = n
	return n, nil
}

// parseStringParam extracts a string parameter from query/body.
func parseStringParam(r *http.Request, bodyBytes []byte, key string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		var m map[string]any
		if json.Unmarshal(bodyBytes, &m) == nil {
			if v, ok := m[key]; ok {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"),
		strings.HasPrefix(ct, "multipart/form-data"):
		_ = r.ParseForm()
		return r.FormValue(key)
	}
	return ""
}

// parseChatID extracts chat_id as int64 from query/body.
func parseChatID(r *http.Request, bodyBytes []byte) int64 {
	if r != nil && r.URL != nil {
		if v := r.URL.Query().Get("chat_id"); v != "" {
			var n int64
			neg := false
			for i, ch := range v {
				if i == 0 && ch == '-' {
					neg = true
					continue
				}
				if ch < '0' || ch > '9' {
					break
				}
				n = n*10 + int64(ch-'0')
			}
			if neg {
				n = -n
			}
			return n
		}
	}
	var ct string
	if r != nil {
		ct = r.Header.Get("Content-Type")
	}
	// Try JSON body first (works when ct empty or application/json).
	if len(bodyBytes) > 0 {
		var m map[string]any
		if json.Unmarshal(bodyBytes, &m) == nil {
			if v, ok := m["chat_id"]; ok {
				switch val := v.(type) {
				case float64:
					return int64(val)
				case json.Number:
					n, _ := val.Int64()
					return n
				case string:
					var n int64
					neg := false
					for i, ch := range val {
						if i == 0 && ch == '-' {
							neg = true
							continue
						}
						if ch < '0' || ch > '9' {
							break
						}
						n = n*10 + int64(ch-'0')
					}
					if neg {
						n = -n
					}
					return n
				}
			}
		}
	}
	// Form-encoded body (parse bytes directly; works without Request).
	if len(bodyBytes) > 0 {
		if vals, err := url.ParseQuery(string(bodyBytes)); err == nil {
			if v := vals.Get("chat_id"); v != "" {
				var n int64
				neg := false
				for i, ch := range v {
					if i == 0 && ch == '-' {
						neg = true
						continue
					}
					if ch < '0' || ch > '9' {
						break
					}
					n = n*10 + int64(ch-'0')
				}
				if neg {
					n = -n
				}
				return n
			}
		}
	}
	_ = ct
	return 0
}

func (f *fakeTG) defaultGetUpdates(w http.ResponseWriter, r *http.Request, token string, bodyBytes []byte) {
	offset := parseIntParam(r, bodyBytes, "offset")
	limit := parseIntParam(r, bodyBytes, "limit")
	timeout := parseIntParam(r, bodyBytes, "timeout")
	if limit <= 0 {
		limit = 100
	}

	f.mu.Lock()
	queue := f.updates[token]
	var result []map[string]any
	for _, upd := range queue {
		uid := 0
		if v, ok := upd["update_id"]; ok {
			switch val := v.(type) {
			case float64:
				uid = int(val)
			case int:
				uid = val
			case int64:
				uid = int(val)
			}
		}
		if uid >= offset {
			result = append(result, upd)
			if len(result) >= limit {
				break
			}
		}
	}
	f.mu.Unlock()

	if len(result) == 0 && timeout > 0 {
		// Short-circuit for tests: wait at most 200ms regardless of timeout
		wait := time.Duration(timeout) * time.Second / 10
		if wait > 200*time.Millisecond {
			wait = 200 * time.Millisecond
		}
		time.Sleep(wait)
	}

	f.writeJSON(w, 200, map[string]any{"ok": true, "result": result})
}

func (f *fakeTG) defaultSend(w http.ResponseWriter, r *http.Request, token, method string, bodyBytes []byte) {
	f.mu.Lock()
	meta, ok := f.botInfo[token]
	f.mu.Unlock()
	if !ok {
		f.writeJSON(w, 401, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}

	chatID := parseChatID(r, bodyBytes)
	text := parseStringParam(r, bodyBytes, "text")
	caption := parseStringParam(r, bodyBytes, "caption")

	msgID := atomic.AddInt64(&f.msgIDCounter, 1)

	result := map[string]any{
		"message_id": msgID,
		"date":       time.Now().Unix(),
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"from":       map[string]any{"id": meta.id, "is_bot": true, "username": meta.username},
	}

	if text != "" {
		result["text"] = text
	}
	if caption != "" {
		result["caption"] = caption
	}

	// Handle media-specific fields
	switch method {
	case "sendPhoto":
		fileID := parseStringParam(r, bodyBytes, "photo")
		if fileID == "" {
			fileID = "photo_" + itoa64(msgID)
		}
		result["photo"] = []map[string]any{
			{
				"file_id":        fileID,
				"file_unique_id": "unique_" + itoa64(msgID),
				"file_size":      100,
				"width":          640,
				"height":         480,
			},
		}
	case "sendVideo":
		fileID := parseStringParam(r, bodyBytes, "video")
		result["video"] = map[string]any{
			"file_id":        fileID,
			"file_unique_id": "unique_" + itoa64(msgID),
			"file_size":      1000,
		}
	case "sendAudio":
		fileID := parseStringParam(r, bodyBytes, "audio")
		result["audio"] = map[string]any{
			"file_id":        fileID,
			"file_unique_id": "unique_" + itoa64(msgID),
			"file_size":      500,
		}
	case "sendDocument":
		fileID := parseStringParam(r, bodyBytes, "document")
		result["document"] = map[string]any{
			"file_id":        fileID,
			"file_unique_id": "unique_" + itoa64(msgID),
			"file_size":      2000,
		}
	case "sendAnimation":
		fileID := parseStringParam(r, bodyBytes, "animation")
		result["animation"] = map[string]any{
			"file_id":        fileID,
			"file_unique_id": "unique_" + itoa64(msgID),
			"file_size":      5000,
		}
	case "sendVoice":
		fileID := parseStringParam(r, bodyBytes, "voice")
		result["voice"] = map[string]any{
			"file_id":        fileID,
			"file_unique_id": "unique_" + itoa64(msgID),
			"duration":       1,
		}
	case "sendSticker":
		fileID := parseStringParam(r, bodyBytes, "sticker")
		result["sticker"] = map[string]any{
			"file_id":        fileID,
			"file_unique_id": "unique_" + itoa64(msgID),
			"width":          512,
			"height":         512,
		}
	case "sendVideoNote":
		fileID := parseStringParam(r, bodyBytes, "video_note")
		result["video_note"] = map[string]any{
			"file_id":        fileID,
			"file_unique_id": "unique_" + itoa64(msgID),
			"length":         240,
			"duration":       10,
		}
	}

	f.writeJSON(w, 200, map[string]any{"ok": true, "result": result})
}

func (f *fakeTG) defaultCopyMessage(w http.ResponseWriter, _ *http.Request, _ string, _ []byte) {
	msgID := atomic.AddInt64(&f.msgIDCounter, 1)
	f.writeJSON(w, 200, map[string]any{
		"ok":     true,
		"result": map[string]any{"message_id": msgID},
	})
}

func (f *fakeTG) defaultForwardMessage(w http.ResponseWriter, r *http.Request, token string, bodyBytes []byte) {
	f.mu.Lock()
	meta, ok := f.botInfo[token]
	f.mu.Unlock()
	if !ok {
		f.writeJSON(w, 401, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}

	chatID := parseChatID(r, bodyBytes)
	msgID := atomic.AddInt64(&f.msgIDCounter, 1)
	f.writeJSON(w, 200, map[string]any{
		"ok": true,
		"result": map[string]any{
			"message_id": msgID,
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"from":       map[string]any{"id": meta.id, "is_bot": true, "username": meta.username},
		},
	})
}

func (f *fakeTG) defaultEditMessageText(w http.ResponseWriter, r *http.Request, token string, bodyBytes []byte) {
	f.mu.Lock()
	meta, ok := f.botInfo[token]
	f.mu.Unlock()
	if !ok {
		f.writeJSON(w, 401, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}

	chatID := parseChatID(r, bodyBytes)
	text := parseStringParam(r, bodyBytes, "text")
	msgID := parseIntParam(r, bodyBytes, "message_id")
	f.writeJSON(w, 200, map[string]any{
		"ok": true,
		"result": map[string]any{
			"message_id": int64(msgID),
			"date":       time.Now().Unix(),
			"chat":       map[string]any{"id": chatID, "type": "private"},
			"from":       map[string]any{"id": meta.id, "is_bot": true, "username": meta.username},
			"text":       text,
		},
	})
}

func (f *fakeTG) defaultGetFile(w http.ResponseWriter, r *http.Request, _ string, bodyBytes []byte) {
	// file_id is used as file_path key per the convention: file_id == path.
	// Accept file_id from query string (GET) or JSON body (POST).
	fileID := parseStringParam(r, bodyBytes, "file_id")

	f.mu.Lock()
	data, ok := f.files[fileID]
	f.mu.Unlock()

	if !ok {
		f.writeJSON(w, 400, map[string]any{"ok": false, "error_code": 400, "description": "file not found"})
		return
	}

	f.writeJSON(w, 200, map[string]any{
		"ok": true,
		"result": map[string]any{
			"file_id":   fileID,
			"file_path": fileID,
			"file_size": len(data),
		},
	})
}

// itoa64 converts int64 to a decimal string without fmt dependency.
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// ---------------------------------------------------------------------------
// Smoke test
// ---------------------------------------------------------------------------

func TestFakeTG_Smoke(t *testing.T) {
	f := newFakeTG(t)
	f.RegisterBot("test123:token", "testbot", 42)

	resp, err := http.Get(f.URL() + "/bot" + "test123:token" + "/getMe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if !out["ok"].(bool) {
		t.Fatalf("getMe not ok: %v", out)
	}

	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %v", out["result"])
	}
	if result["username"] != "testbot" {
		t.Fatalf("expected username=testbot, got %v", result["username"])
	}
	if result["id"].(float64) != 42 {
		t.Fatalf("expected id=42, got %v", result["id"])
	}
}
