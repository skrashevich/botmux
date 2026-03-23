package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	githubRepo         = "skrashevich/botmux"
	updateCheckInterval = 6 * time.Hour
)

type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

type UpdateCheck struct {
	Current          string `json:"current_version"`
	Latest           string `json:"latest_version,omitempty"`
	UpdateAvailable  bool   `json:"update_available"`
	ReleaseURL       string `json:"release_url,omitempty"`
	CheckedAt        string `json:"checked_at,omitempty"`
	Error            string `json:"error,omitempty"`
}

type VersionChecker struct {
	mu          sync.Mutex
	lastCheck   time.Time
	cachedCheck *UpdateCheck
}

func NewVersionChecker() *VersionChecker {
	return &VersionChecker{}
}

func (vc *VersionChecker) GetVersionInfo() VersionInfo {
	return VersionInfo{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
	}
}

func (vc *VersionChecker) CheckForUpdate() UpdateCheck {
	vc.mu.Lock()
	defer vc.mu.Unlock()

	if vc.cachedCheck != nil && time.Since(vc.lastCheck) < updateCheckInterval {
		return *vc.cachedCheck
	}

	result := vc.fetchLatestRelease()
	vc.cachedCheck = &result
	vc.lastCheck = time.Now()
	return result
}

func (vc *VersionChecker) fetchLatestRelease() UpdateCheck {
	result := UpdateCheck{
		Current: version,
	}

	if version == "dev" || version == "unknown" {
		result.Error = "development build, skipping update check"
		return result
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/" + githubRepo + "/releases/latest")
	if err != nil {
		result.Error = "failed to check for updates"
		log.Printf("Version check failed: %v", err)
		return result
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		result.Error = "no releases found"
		return result
	}
	if resp.StatusCode == http.StatusForbidden {
		result.Error = "GitHub API rate limit exceeded"
		return result
	}
	if resp.StatusCode != http.StatusOK {
		result.Error = "unexpected response from GitHub"
		return result
	}

	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		result.Error = "failed to parse release info"
		return result
	}

	result.Latest = release.TagName
	if strings.HasPrefix(release.HTMLURL, "https://github.com/"+githubRepo) {
		result.ReleaseURL = release.HTMLURL
	} else {
		result.ReleaseURL = "https://github.com/" + githubRepo + "/releases"
	}
	result.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	result.UpdateAvailable = compareSemver(version, release.TagName) < 0

	return result
}

// compareSemver compares two semver strings (with optional "v" prefix).
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareSemver(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	partsA := strings.SplitN(a, ".", 3)
	partsB := strings.SplitN(b, ".", 3)

	for i := range max(len(partsA), len(partsB)) {
		var va, vb int
		if i < len(partsA) {
			// Strip pre-release suffix (e.g., "1-beta" -> "1")
			va, _ = strconv.Atoi(strings.SplitN(partsA[i], "-", 2)[0])
		}
		if i < len(partsB) {
			vb, _ = strconv.Atoi(strings.SplitN(partsB[i], "-", 2)[0])
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}
