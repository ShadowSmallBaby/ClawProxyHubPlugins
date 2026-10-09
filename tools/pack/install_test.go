package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

func TestInstallLuaWithoutCoreCheckoutOrCompiler(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	const source = "plugins-lua/example"
	if err := os.MkdirAll(filepath.Join(source, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"manifest.json":  `{"name":"example","version":"1.0.0","author":"cph","label":{"zh":"示例","en":"Example"},"icon":"icon.png"}`,
		"main.lua":       `return require("lib.helper")`,
		"lib/helper.lua": "return {}",
		"icon.png":       "icon",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := installDev("example", "installed"); err != nil {
		t.Fatal(err)
	}
	const target = "installed/example"
	for name, expected := range files {
		if name == "manifest.json" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(content) != expected {
			t.Fatalf("installed %s = %q, error %v", name, content, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(target, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var installed manifest
	if err := json.Unmarshal(raw, &installed); err != nil {
		t.Fatal(err)
	}
	if installed.Runtime != "lua" || installed.Entry != "main.lua" || installed.ProtocolVersion != sdk.ProtocolVersion {
		t.Fatalf("invalid Lua manifest: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(target, binaryName(runtime.GOOS, runtime.GOARCH))); !os.IsNotExist(err) {
		t.Fatalf("unexpected per-plugin runtime: %v", err)
	}
}

func TestInstallLuaReportsAssetWriteFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, dir := range []string{"plugins-lua/example/lib", "installed/example"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"plugins-lua/example/manifest.json":  `{"name":"example","version":"1.0.0","author":"cph","label":{"zh":"示例","en":"Example"}}`,
		"plugins-lua/example/main.lua":       "return {}",
		"plugins-lua/example/lib/helper.lua": "return {}",
		"installed/example/lib":              "blocks the library directory",
	} {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := installDev("example", "installed"); err == nil {
		t.Fatal("installation succeeded despite an unwritable library path")
	}
}
