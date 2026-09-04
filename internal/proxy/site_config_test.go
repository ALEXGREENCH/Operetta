package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSiteConfigMatchesHostnameWithoutPortAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "127.0.0.1.json")
	write := func(value string, stamp time.Time) {
		t.Helper()
		data := []byte(`{"headers":{"X-Template":"` + value + `"}}`)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	write("first", stamp)
	store := newSiteConfigStore(dir)
	first := store.Find("http://127.0.0.1:18081/page")
	if first == nil || first.Headers["X-Template"] != "first" {
		t.Fatalf("first config = %+v", first)
	}

	write("second", stamp.Add(time.Second))
	second := store.Find("http://127.0.0.1:18081/page")
	if second == nil || second.Headers["X-Template"] != "second" {
		t.Fatalf("reloaded config = %+v", second)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if removed := store.Find("http://127.0.0.1:18081/page"); removed != nil {
		t.Fatalf("removed config remained cached: %+v", removed)
	}
}

func TestSiteConfigFallsBackFromSubdomainToParent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "example.test.json"), []byte(`{"headers":{"X-Parent":"yes"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := newSiteConfigStore(dir).Find("https://m.example.test/path")
	if cfg == nil || cfg.Headers["X-Parent"] != "yes" {
		t.Fatalf("parent config = %+v", cfg)
	}
}
