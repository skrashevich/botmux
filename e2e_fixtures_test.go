package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixturesMatchSnapshot(t *testing.T) {
	snapshotPath := filepath.Join("testdata", "tg", "_spec_snapshot.sha256")
	f, err := os.Open(snapshotPath)
	if err != nil {
		t.Fatalf("cannot open snapshot: %v", err)
	}
	defer f.Close()

	expected := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Format: "<hash>  <filename>" (sha256sum style, two spaces)
		parts := strings.Fields(line)
		if len(parts) != 2 {
			t.Fatalf("malformed snapshot line: %q", line)
		}
		expected[parts[1]] = parts[0]
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	for filename, expectedHash := range expected {
		path := filepath.Join("testdata", "tg", filename)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("cannot read %s: %v", filename, err)
			continue
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != expectedHash {
			t.Errorf("fixture %s hash mismatch\n  expected: %s\n  got:      %s\nIf fixture was intentionally updated, follow procedure in testdata/tg/README.md",
				filename, expectedHash, got)
		}
	}
}

func TestFixturesJSONValid(t *testing.T) {
	dir := filepath.Join("testdata", "tg")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read testdata/tg: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("cannot read %s: %v", e.Name(), err)
			continue
		}
		if !json.Valid(data) {
			t.Errorf("invalid JSON in %s", e.Name())
		}
	}
}
