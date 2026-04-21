package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	verpkg "github.com/skrashevich/botmux/internal/version"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/proxy"
	"github.com/skrashevich/botmux/internal/server"
)

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.1.0", -1},
		{"v1.1.0", "v1.0.0", 1},
		{"v1.0.0", "v1.0.0", 0},
		{"1.0.0", "1.0.0", 0},
		{"v1.9.0", "v1.10.0", -1},
		{"v2.0.0", "v1.99.99", 1},
		{"v0.1.0", "v0.2.0", -1},
		{"v1.0", "v1.0.0", 0},
		{"v1.0.0-beta", "v1.0.0-rc1", 0},
		{"v1.0.0", "v1.0.1", -1},
		{"v10.0.0", "v9.0.0", 1},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_vs_%s", tt.a, tt.b), func(t *testing.T) {
			got := verpkg.CompareSemver(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("verpkg.CompareSemver(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestVersionCheckerGetVersionInfo(t *testing.T) {
	vc := verpkg.NewChecker("dev", "unknown", "unknown")
	info := vc.GetVersionInfo()

	if info.Version != "dev" {
		t.Errorf("expected version %q, got %q", "dev", info.Version)
	}
	if info.Commit != "unknown" {
		t.Errorf("expected commit %q, got %q", "unknown", info.Commit)
	}
	if info.BuildDate != "unknown" {
		t.Errorf("expected buildDate %q, got %q", "unknown", info.BuildDate)
	}
}

func TestVersionCheckerSkipsDevBuilds(t *testing.T) {
	vc := verpkg.NewChecker("dev", "unknown", "unknown")
	// version is "dev" in tests (no ldflags)
	result := vc.CheckForUpdate()

	if result.Current != "dev" {
		t.Errorf("expected current=dev, got %q", result.Current)
	}
	if result.UpdateAvailable {
		t.Error("dev build should not report update available")
	}
	if result.Error == "" {
		t.Error("dev build should have an error message about skipping")
	}
}

func TestVersionCheckerCachesResult(t *testing.T) {
	vc := verpkg.NewChecker("dev", "unknown", "unknown")

	r1 := vc.CheckForUpdate()
	r2 := vc.CheckForUpdate()

	// Both should return same result (cached)
	if r1.Current != r2.Current || r1.Error != r2.Error {
		t.Error("second call should return cached result")
	}
}

func TestHealthEndpointNoVersionInfo(t *testing.T) {
	store := newTestStore(t)
	server := server.NewServer(store, nil)

	mux := server.BuildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var data map[string]string
	json.NewDecoder(resp.Body).Decode(&data)

	if data["status"] != "ok" {
		t.Errorf("expected status=ok, got %q", data["status"])
	}
	// Health should NOT expose version info
	if _, ok := data["version"]; ok {
		t.Error("health endpoint should not expose version field")
	}
	if _, ok := data["commit"]; ok {
		t.Error("health endpoint should not expose commit field")
	}
}

func TestHealthEndpointDemoMode(t *testing.T) {
	store := newTestStore(t)
	server := server.NewServer(store, nil)
	server.DemoMode = true

	mux := server.BuildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var data map[string]string
	json.NewDecoder(resp.Body).Decode(&data)

	if data["mode"] != "demo" {
		t.Errorf("expected mode=demo, got %q", data["mode"])
	}
}

func TestVersionEndpointRequiresAuth(t *testing.T) {
	store := newTestStore(t)
	server := server.NewServer(store, nil)

	mux := server.BuildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// No auth — should get 401
	resp, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 401 {
		t.Errorf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

func TestVersionEndpointWithAuth(t *testing.T) {
	store := newTestStore(t)
	server := server.NewServer(store, nil)
	server.VersionChecker = verpkg.NewChecker("dev", "unknown", "unknown")

	session := createTestAuth(t, store)

	mux := server.BuildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/version", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var data map[string]json.RawMessage
	json.NewDecoder(resp.Body).Decode(&data)

	if _, ok := data["version"]; !ok {
		t.Error("response should contain 'version' field")
	}
	if _, ok := data["update"]; !ok {
		t.Error("response should contain 'update' field")
	}
}

func TestIsValidHexColor(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"#ff0000", true},
		{"#FFF", true},
		{"#00f0ff", true},
		{"#aabbccdd", true},
		{"#1234", true},
		{"", false},
		{"ff0000", false},
		{"#xyz", false},
		{"#ff", false},
		{`#ff" onmouseover="alert(1)`, false},
		{"red", false},
		{"#", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := server.IsValidHexColor(tt.input)
			if got != tt.want {
				t.Errorf("server.IsValidHexColor(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestUserProfileEndpointRequiresAuth(t *testing.T) {
	store := newTestStore(t)
	proxy := proxy.NewManager(store, "https://api.telegram.org")
	server := server.NewServer(store, proxy)

	mux := server.BuildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/users/profile?bot_id=1&chat_id=123&user_id=456")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 401 {
		t.Errorf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

func TestUserProfileEndpointValidation(t *testing.T) {
	store := newTestStore(t)
	proxy := proxy.NewManager(store, "https://api.telegram.org")
	server := server.NewServer(store, proxy)
	session := createTestAuth(t, store)

	mux := server.BuildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Missing chat_id and user_id — should get 400
	req, _ := http.NewRequest("GET", ts.URL+"/api/users/profile?bot_id=1", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for missing params, got %d", resp.StatusCode)
	}
}

func TestUserProfileEndpointReturnsData(t *testing.T) {
	store := newTestStore(t)
	proxy := proxy.NewManager(store, "https://api.telegram.org")
	server := server.NewServer(store, proxy)
	session := createTestAuth(t, store)

	// Create a bot
	botID, err := store.AddBotConfig(models.BotConfig{
		Name:        "Profile Bot",
		Token:       "profile-token",
		BotUsername: "profilebot",
	})
	if err != nil {
		t.Fatalf("AddBotConfig error: %v", err)
	}

	// Track a user
	store.TrackUser(123, 456, "@testuser")

	// Save some messages from this user
	for i := 1; i <= 3; i++ {
		store.SaveMessage(models.Message{
			ID:       i,
			BotID:    botID,
			ChatID:   123,
			FromUser: "@testuser",
			FromID:   456,
			Text:     fmt.Sprintf("message %d", i),
			Date:     int64(1700000000 + i),
		})
	}

	// Add a tag
	store.AddUserTag(models.UserTag{
		ChatID:   123,
		UserID:   456,
		Username: "@testuser",
		Tag:      "VIP",
		Color:    "#00ff00",
	})

	mux := server.BuildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/users/profile?bot_id=%d&chat_id=123&user_id=456", ts.URL, botID), nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var profile map[string]any
	json.NewDecoder(resp.Body).Decode(&profile)

	if profile["user_id"].(float64) != 456 {
		t.Errorf("expected user_id=456, got %v", profile["user_id"])
	}
	if profile["chat_id"].(float64) != 123 {
		t.Errorf("expected chat_id=123, got %v", profile["chat_id"])
	}

	// Should have tags
	tags, ok := profile["tags"].([]any)
	if !ok || len(tags) != 1 {
		t.Errorf("expected 1 tag, got %v", profile["tags"])
	}
}

func TestUserProfileEndpointBotAccessCheck(t *testing.T) {
	store := newTestStore(t)
	proxy := proxy.NewManager(store, "https://api.telegram.org")
	server := server.NewServer(store, proxy)

	// Create a non-admin user
	_, err := store.CreateUser("regular", "password123", "Regular User", "user")
	if err != nil {
		t.Fatalf("CreateUser error: %v", err)
	}
	user, err := store.GetUserByUsername("regular")
	if err != nil {
		t.Fatalf("GetUserByUsername error: %v", err)
	}

	// Create session for non-admin user
	token, _ := auth.GenerateSessionToken()
	store.CreateSession(token, user.ID, time.Now().Add(24*time.Hour))

	// Create a bot (not assigned to this user)
	botID, _ := store.AddBotConfig(models.BotConfig{
		Name:        "Restricted Bot",
		Token:       "restricted-token",
		BotUsername: "restricted",
	})

	mux := server.BuildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/users/profile?bot_id=%d&chat_id=123&user_id=456", ts.URL, botID), nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 403 {
		t.Errorf("expected 403 for unauthorized bot access, got %d", resp.StatusCode)
	}
}
