package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/proxy"
	"github.com/skrashevich/botmux/internal/server"
)

func newAuthTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st := newTestStore(t)
	pm := proxy.NewManager(st, "https://api.telegram.org")
	srv := server.NewServer(st, pm)
	ts := httptest.NewServer(srv.BuildMux())
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, client *http.Client, url string, body any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&data)
	return resp, data
}

func sessionCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	t.Fatalf("session cookie %q not set; got %v", auth.SessionCookieName, resp.Cookies())
	return nil
}

// Regression for https://github.com/skrashevich/botmux/issues/43:
// over plain HTTP the session cookie must not carry the Secure attribute,
// otherwise browsers drop it and the forced first-login password change
// silently fails (401 -> back to login screen, old password still active).
func TestFirstLoginPasswordChangeOverPlainHTTP(t *testing.T) {
	ts := newAuthTestServer(t)

	// cookiejar behaves like a browser: Secure cookies are never sent over http://.
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	resp, data := postJSON(t, client, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "admin"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("initial login: expected 200, got %d (%v)", resp.StatusCode, data)
	}
	if data["must_change_password"] != true {
		t.Fatalf("expected must_change_password=true on first login, got %v", data["must_change_password"])
	}
	if c := sessionCookie(t, resp); c.Secure {
		t.Fatalf("session cookie must not be Secure over plain HTTP")
	}

	resp, data = postJSON(t, client, ts.URL+"/api/auth/change-password",
		map[string]string{"old_password": "", "new_password": "newpass123"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("change-password: expected 200, got %d (%v)", resp.StatusCode, data)
	}

	// New password works and the forced-change flag is cleared.
	resp, data = postJSON(t, client, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "newpass123"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login with new password: expected 200, got %d (%v)", resp.StatusCode, data)
	}
	if data["must_change_password"] != false {
		t.Fatalf("expected must_change_password=false after change, got %v", data["must_change_password"])
	}

	// Old password is rejected.
	resp, _ = postJSON(t, client, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "admin"}, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("login with old password: expected 401, got %d", resp.StatusCode)
	}
}

func TestSessionCookieSecureFlagFollowsRequestScheme(t *testing.T) {
	ts := newAuthTestServer(t)
	client := &http.Client{}
	creds := map[string]string{"username": "admin", "password": "admin"}

	cases := []struct {
		name    string
		headers map[string]string
		secure  bool
	}{
		{"plain http", nil, false},
		{"forwarded https", map[string]string{"X-Forwarded-Proto": "https"}, true},
		{"forwarded https uppercase", map[string]string{"X-Forwarded-Proto": "HTTPS"}, true},
		{"forwarded chain https first", map[string]string{"X-Forwarded-Proto": "https, http"}, true},
		{"forwarded http", map[string]string{"X-Forwarded-Proto": "http"}, false},
		{"rfc7239 forwarded https", map[string]string{"Forwarded": "for=10.0.0.1;proto=https"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := postJSON(t, client, ts.URL+"/api/auth/login", creds, tc.headers)
			if resp.StatusCode != 200 {
				t.Fatalf("login: expected 200, got %d", resp.StatusCode)
			}
			if got := sessionCookie(t, resp).Secure; got != tc.secure {
				t.Fatalf("Secure=%v, want %v", got, tc.secure)
			}
		})
	}

	// Logout must clear the cookie using the same scheme-dependent attribute.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/logout", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if c := sessionCookie(t, resp); !c.Secure || c.MaxAge >= 0 {
		t.Fatalf("logout cookie: Secure=%v MaxAge=%d, want Secure=true and MaxAge<0", c.Secure, c.MaxAge)
	}
}
