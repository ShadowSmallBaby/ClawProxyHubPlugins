// pack — 插件打包器：交叉编译 + 生成 .cphplugin 包与市场索引 index.json。
//
// 用法：
//
//	go run ./tools/pack                                   # 全部插件 → dist/
//	go run ./tools/pack -only lobsterai                   # 只打包指定插件
//	go run ./tools/pack -skip lobsterai                   # 跳过已发布版本，索引条目沿用现有 index.json
//	go run ./tools/pack -install ../ClawProxyHub/data/plugins   # 本地开发：编译当前平台并装入核心插件目录
//
// 包格式（zip）：manifest.json + 图标 + plugin-<os>-<arch>[.exe]（多平台）。
// 版本号唯一来源是 plugins/<name>/manifest.json，经 -ldflags 注入二进制（Handshake 回报同一值）；
// protocol_version 取自编译所用 SDK，保证包声明与二进制实际握手版本一致。
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ShadowSmallBaby/ClawProxyHub/sdk"
)

const defaultBaseURL = "https://github.com/ShadowSmallBaby/ClawProxyHubPlugins/releases/download"

// platforms 发布包内置的目标平台（核心安装时按 runtime.GOOS/GOARCH 挑选）。
var platforms = [][2]string{
	{"windows", "amd64"},
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
}

// zipTime 包内固定时间戳：同一份输入产出字节一致的包，sha256 可复现。
var zipTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// manifest 插件清单：源文件 <root>/<name>/manifest.json + 打包时补齐 protocol_version。
// runtime=="lua" 的脚本插件保留声明的 protocol_version（不被 SDK 覆盖），并带 entry。
type manifest struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	Author          string            `json:"author"`
	ProtocolVersion int32             `json:"protocol_version"`
	Runtime         string            `json:"runtime,omitempty"`
	Entry           string            `json:"entry,omitempty"`
	Label           map[string]string `json:"label,omitempty"`
	Icon            string            `json:"icon,omitempty"`
}

// indexEntry 市场 index.json 单条目（字段与核心 MarketEntry 对齐）。
type indexEntry struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Author      string            `json:"author,omitempty"`
	Runtime     string            `json:"runtime,omitempty"` // lua 等脚本插件；空=Go 插件
	Label       map[string]string `json:"label"`
	PublishedAt string            `json:"published_at,omitempty"`
	DownloadURL string            `json:"download_url"`
	SHA256      string            `json:"sha256"`
}

func main() {
	out := flag.String("out", "dist", "包与 index.json 的输出目录")
	baseURL := flag.String("base-url", defaultBaseURL, "下载地址前缀：<base-url>/<name>-v<version>/<name>-<version>.cphplugin")
	indexPath := flag.String("index", "index.json", "现有索引路径（-skip 的插件从这里沿用条目）")
	only := flag.String("only", "", "只处理这些插件（逗号分隔）")
	skip := flag.String("skip", "", "跳过这些插件的构建（逗号分隔，通常是已发布版本）")
	install := flag.String("install", "", "开发模式：只编译当前平台并安装到该插件目录（不打包、不生成索引）")
	flag.Parse()

	if err := run(*out, *baseURL, *indexPath, csv(*only), csv(*skip), *install); err != nil {
		fmt.Fprintln(os.Stderr, "pack:", err)
		os.Exit(1)
	}
}

func run(out, baseURL, indexPath string, only, skip map[string]bool, install string) error {
	names, err := discover(only)
	if err != nil {
		return err
	}

	if install != "" {
		for _, name := range names {
			if err := installDev(name, install); err != nil {
				return err
			}
		}
		return nil
	}

	existing, err := readIndex(indexPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	var entries []indexEntry
	for _, name := range names {
		if skip[name] {
			e, ok := existing[name]
			if !ok {
				fmt.Fprintf(os.Stderr, "warn: %s skipped but absent from %s, dropped from index\n", name, indexPath)
				continue
			}
			fmt.Printf("skip %s (keep %s v%s)\n", name, name, e.Version)
			entries = append(entries, e)
			continue
		}
		e, err := pack(name, out, baseURL)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		entries = append(entries, e)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	data, _ := json.MarshalIndent(entries, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "index.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("index: %d entries -> %s\n", len(entries), filepath.Join(out, "index.json"))
	return nil
}

// pluginRoots 扫描的插件根：Go 插件在 plugins/，Lua 等脚本插件在 plugins-lua/。
var pluginRoots = []string{"plugins", "plugins-lua"}

// pluginRoot 定位插件所在根目录（插件名全局唯一，找不到默认 plugins）。
func pluginRoot(name string) string {
	for _, root := range pluginRoots {
		if _, err := os.Stat(filepath.Join(root, name, "manifest.json")); err == nil {
			return root
		}
	}
	return "plugins"
}

// discover 列出 plugins/ 与 plugins-lua/ 下带 manifest.json 的插件（-only 过滤）。
func discover(only map[string]bool) ([]string, error) {
	var names []string
	for _, root := range pluginRoots {
		dirs, err := os.ReadDir(root)
		if err != nil {
			continue // 根不存在则跳过（plugins-lua 可能尚无插件）
		}
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, d.Name(), "manifest.json")); err != nil {
				continue
			}
			if len(only) > 0 && !only[d.Name()] {
				continue
			}
			names = append(names, d.Name())
		}
	}
	for n := range only {
		if !contains(names, n) {
			return nil, fmt.Errorf("plugin %q not found (need plugins/%s or plugins-lua/%s manifest.json)", n, n, n)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no plugins found")
	}
	return names, nil
}

// pack 打包一个插件：全平台交叉编译 → zip → sha256 → 索引条目。
func pack(name, out, baseURL string) (indexEntry, error) {
	mf, err := loadManifest(name)
	if err != nil {
		return indexEntry{}, err
	}
	stage, err := os.MkdirTemp("", "cph-pack-"+name+"-")
	if err != nil {
		return indexEntry{}, err
	}
	defer os.RemoveAll(stage)

	// 包内条目：(包内名, 本地路径)，排序后写入保证顺序稳定。
	// lua 等脚本插件产平台无关包（只含脚本）；Go 插件全平台交叉编译。
	files := map[string]string{}
	if mf.Runtime == "lua" {
		if err := collectLuaFiles(name, files); err != nil {
			return indexEntry{}, err
		}
	} else {
		for _, p := range platforms {
			bin := binaryName(p[0], p[1])
			path := filepath.Join(stage, bin)
			if err := goBuild(name, mf.Version, p[0], p[1], path); err != nil {
				return indexEntry{}, err
			}
			files[bin] = path
		}
	}
	mfPath := filepath.Join(stage, "manifest.json")
	if err := writeManifest(mf, mfPath); err != nil {
		return indexEntry{}, err
	}
	files["manifest.json"] = mfPath
	if mf.Icon != "" {
		files[mf.Icon] = filepath.Join(pluginRoot(name), name, mf.Icon)
	}

	pkg := filepath.Join(out, fmt.Sprintf("%s-%s.cphplugin", name, mf.Version))
	sum, err := writeZip(pkg, files)
	if err != nil {
		return indexEntry{}, err
	}
	fmt.Printf("packed %s (v%s, %d entries, sha256 %s)\n", pkg, mf.Version, len(files), sum[:12])

	return indexEntry{
		Name: mf.Name, Version: mf.Version, Author: mf.Author, Label: mf.Label,
		Runtime:     mf.Runtime,
		PublishedAt: time.Now().UTC().Format("2006-01-02"),
		DownloadURL: fmt.Sprintf("%s/%s-v%s/%s-%s.cphplugin", strings.TrimSuffix(baseURL, "/"), name, mf.Version, name, mf.Version),
		SHA256:      sum,
	}, nil
}

// installDev 开发安装：当前平台二进制 + manifest + 图标 → <dir>/<name>/（与核心目录约定一致）。
// lua 插件注入本地编译的 luahost 作为 plugin-<os>-<arch>，脚本原样落地（方案 A 的开发版）。
func installDev(name, dir string) error {
	mf, err := loadManifest(name)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	root := filepath.Join(pluginRoot(name), name)
	bin := filepath.Join(target, binaryName(runtime.GOOS, runtime.GOARCH))
	if mf.Runtime == "lua" {
		if err := buildLuahost(bin); err != nil {
			return fmt.Errorf("%s: build luahost: %w（核心运行中会锁住二进制，先在插件页停止该插件）", name, err)
		}
		if err := copyFile(filepath.Join(root, "main.lua"), filepath.Join(target, "main.lua")); err != nil {
			return err
		}
		if libs, err := os.ReadDir(filepath.Join(root, "lib")); err == nil {
			_ = os.MkdirAll(filepath.Join(target, "lib"), 0o755)
			for _, e := range libs {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".lua") {
					_ = copyFile(filepath.Join(root, "lib", e.Name()), filepath.Join(target, "lib", e.Name()))
				}
			}
		}
	} else if err := goBuild(name, mf.Version, runtime.GOOS, runtime.GOARCH, bin); err != nil {
		return fmt.Errorf("%s: %w（核心运行中会锁住二进制，先在插件页停止该插件）", name, err)
	}
	if err := writeManifest(mf, filepath.Join(target, "manifest.json")); err != nil {
		return err
	}
	if mf.Icon != "" {
		if err := copyFile(filepath.Join(root, mf.Icon), filepath.Join(target, mf.Icon)); err != nil {
			return err
		}
	}
	fmt.Printf("installed %s v%s -> %s\n", name, mf.Version, bin)
	return nil
}

// loadManifest 读源清单并校验必填项，再由 pack 盖章机械字段（作者只需写 name/version/author/label/icon）：
// runtime 由目录推断（plugins-lua/ → lua）、entry 固定 main.lua、protocol_version 由 SDK 盖
// （Go/lua 同跑一份 SDK 契约，值一致；与二进制/luahost 握手对齐）。
func loadManifest(name string) (*manifest, error) {
	root := pluginRoot(name)
	data, err := os.ReadFile(filepath.Join(root, name, "manifest.json"))
	if err != nil {
		return nil, err
	}
	mf := &manifest{}
	if err := json.Unmarshal(data, mf); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	if mf.Name != name {
		return nil, fmt.Errorf("manifest name %q != directory %q", mf.Name, name)
	}
	if mf.Version == "" || mf.Author == "" {
		return nil, fmt.Errorf("manifest.json requires version and author")
	}
	if mf.Icon != "" {
		if _, err := os.Stat(filepath.Join(root, name, mf.Icon)); err != nil {
			return nil, fmt.Errorf("icon %q: %w", mf.Icon, err)
		}
	}
	// 目录即身份：plugins-lua/ 下一律 lua，脚本入口固定 main.lua。
	if root == "plugins-lua" {
		mf.Runtime = "lua"
		if mf.Entry == "" {
			mf.Entry = "main.lua"
		}
	}
	mf.ProtocolVersion = sdk.ProtocolVersion
	return mf, nil
}

func writeManifest(mf *manifest, path string) error {
	data, _ := json.MarshalIndent(mf, "", "  ")
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// collectLuaFiles 收集脚本插件包内文件：入口 main.lua + 可选 lib/*.lua（manifest/icon 由 pack 统一补）。
func collectLuaFiles(name string, files map[string]string) error {
	root := filepath.Join(pluginRoot(name), name)
	entry := filepath.Join(root, "main.lua")
	if _, err := os.Stat(entry); err != nil {
		return fmt.Errorf("lua plugin missing main.lua: %w", err)
	}
	files["main.lua"] = entry
	if libs, err := os.ReadDir(filepath.Join(root, "lib")); err == nil {
		for _, e := range libs {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".lua") {
				files["lib/"+e.Name()] = filepath.Join(root, "lib", e.Name())
			}
		}
	}
	return nil
}

// buildLuahost 编译主仓 hosts/luahost（当前平台，纯 Go）到 out；pack 在插件仓根运行，主仓在 ../。
func buildLuahost(out string) error {
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", abs, ".")
	cmd.Dir = filepath.Join("..", "hosts", "luahost")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// goBuild 交叉编译单个插件；CGO 关闭 + trimpath 保证产物可复现。
func goBuild(name, version, goos, goarch, out string) error {
	cmd := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-s -w -X main.version="+version,
		"-o", out, "./"+pluginRoot(name)+"/"+name)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %s/%s: %w", goos, goarch, err)
	}
	return nil
}

func binaryName(goos, goarch string) string {
	n := fmt.Sprintf("plugin-%s-%s", goos, goarch)
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

// writeZip 写确定性 zip（固定时间戳、按名排序），返回文件 sha256。
func writeZip(path string, files map[string]string) (string, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, n := range names {
		hdr := &zip.FileHeader{Name: n, Method: zip.Deflate, Modified: zipTime}
		if strings.HasPrefix(n, "plugin-") {
			hdr.SetMode(0o755)
		} else {
			hdr.SetMode(0o644)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return "", err
		}
		src, err := os.Open(files[n])
		if err != nil {
			return "", err
		}
		_, err = io.Copy(w, src)
		src.Close()
		if err != nil {
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return fileSHA256(path)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readIndex 读现有索引（不存在视为空）。
func readIndex(path string) (map[string]indexEntry, error) {
	out := map[string]indexEntry{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []indexEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, e := range entries {
		out[e.Name] = e
	}
	return out, nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func csv(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
