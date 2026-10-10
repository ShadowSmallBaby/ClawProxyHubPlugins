package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSkipPreservesReleaseAndPlatformMetadata(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("plugins-go/example", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("plugins-go/example/manifest.json", []byte(`{"name":"example","version":"1.0.0"}`), 0644); err != nil {
		t.Fatal(err)
	}
	original := `[{"name":"example","version":"1.0.0","download_url":"https://example.com/desktop","sha256":"desktop","release_manifest":{"download_url":"https://example.com/manifest.json","sha256":"manifest"},"protocol_version":2,"platforms":{"android":["arm64-v8a"],"ios":[]},"android":{"arm64-v8a":{"download_url":"https://example.com/obsolete"}}}]`
	if err := os.WriteFile("index.json", []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run("out", "https://unused.example", "index.json", nil, map[string]bool{"example": true}, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("out", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []indexEntry
	if err = json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	var fields []map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(entries[0].ReleaseManifest) == 0 || entries[0].ProtocolVersion != 2 || entries[0].SHA256 != "desktop" || entries[0].DownloadURL != "https://example.com/desktop" || len(fields[0]["android"]) != 0 {
		t.Fatal(string(raw))
	}
	if len(entries[0].Platforms["android"]) != 1 || entries[0].Platforms["android"][0] != "arm64-v8a" || entries[0].Platforms["ios"] == nil || len(entries[0].Platforms["ios"]) != 0 {
		t.Fatal(string(raw))
	}
}
