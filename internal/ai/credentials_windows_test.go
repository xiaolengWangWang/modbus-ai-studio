//go:build windows

package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectedKeyRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "deepseek.key")
	if err := SaveKey(path, "test-only-secret"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "test-only-secret") {
		t.Fatal("plaintext persisted")
	}
	key, err := LoadKey(path)
	if err != nil || key != "test-only-secret" {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if err := SaveKey(path, "replacement-secret"); err != nil {
		t.Fatal(err)
	}
	key, err = LoadKey(path)
	if err != nil || key != "replacement-secret" {
		t.Fatalf("replacement failed: %v", err)
	}
	if err := DeleteKey(path); err != nil {
		t.Fatal(err)
	}
	if key, err := LoadKey(path); err != nil || key != "" {
		t.Fatalf("deleted credential remains: %v", err)
	}
}
