package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type contextKey string

const authUserKey contextKey = "auth_user"

type AuthUser struct {
	ID                 int64  `json:"id"`
	Username           string `json:"username"`
	PasswordHash       string `json:"-"`
	DisplayName        string `json:"display_name"`
	Role               string `json:"role"` // "admin" or "user"
	MustChangePassword bool   `json:"must_change_password"`
	CreatedAt          string `json:"created_at"`
	LastLogin          string `json:"last_login"`
}

const sessionCookieName = "botmux_session"
const sessionDuration = 30 * 24 * time.Hour

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func GenerateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func GenerateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "bmx_" + hex.EncodeToString(b), nil
}

func HashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// authMiddleware checks Bearer API key or session cookie and adds user to context
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var user *AuthUser

		// Check Bearer token first
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			keyHash := HashAPIKey(token)
			u, err := s.store.GetUserByAPIKey(keyHash)
			if err == nil && u != nil {
				user = u
			}
		}

		// Fall back to session cookie
		if user == nil {
			cookie, err := r.Cookie(sessionCookieName)
			if err != nil || cookie.Value == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			u, err := s.store.GetUserBySession(cookie.Value)
			if err != nil || u == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			user = u
		}

		ctx := context.WithValue(r.Context(), authUserKey, user)
		next(w, r.WithContext(ctx))
	}
}

// adminOnly requires admin role
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		user := getAuthUser(r)
		if user == nil || user.Role != "admin" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			w.Write([]byte(`{"error":"forbidden"}`))
			return
		}
		next(w, r)
	})
}

func getAuthUser(r *http.Request) *AuthUser {
	if u, ok := r.Context().Value(authUserKey).(*AuthUser); ok {
		return u
	}
	return nil
}

// checkBotAccess verifies user has access to the bot (admin = all, user = assigned only)
func (s *Server) checkBotAccess(r *http.Request, botID int64) bool {
	user := getAuthUser(r)
	if user == nil {
		return false
	}
	if user.Role == "admin" {
		return true
	}
	return s.store.UserHasBotAccess(user.ID, botID)
}
