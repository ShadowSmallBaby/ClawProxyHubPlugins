# 插件包与市场清单

业务插件使用 `.cphplugin`。功能扩展 `.cphext` 采用另一套元数据与签名协议。

| 文件 | 位置 | 用途 |
| --- | --- | --- |
| 包内 `manifest.json` | `.cphplugin` ZIP 内 | 插件身份、运行时、协议和 Android 原生库信息 |
| 发布 `manifest.json` | 每个插件的 GitHub Release 资产 | 各平台包的 URL、SHA-256、大小和格式 |
| `index.json` | 仓库根目录 | 市场目录，每个插件一个条目，指向发布清单和兼容包 |

以下 JSON 为结构示例，URL、哈希、日期和大小用于说明字段，不表示对应资产已发布。版本以插件源目录的 `manifest.json` 为准，`protocol_version` 由构建所用 SDK 注入。

## 包内清单

桌面 Go 包包含清单、可选图标和 `plugin-<os>-<arch>[.exe]`：

```json
{
  "name": "newapi",
  "version": "0.1.6",
  "author": "cph",
  "protocol_version": 2,
  "label": { "zh": "New API", "en": "New API" },
  "icon": "icon.png"
}
```

Go 的 `runtime` 可省略。兼容包包含 Windows amd64、Linux amd64/arm64、macOS amd64/arm64 五个二进制；发布脚本从兼容包拆出各桌面平台包。

Android Go 包为每个 ABI 独立打包，显式声明 `runtime: "go"`：

```json
{
  "name": "newapi",
  "version": "0.1.6",
  "author": "cph",
  "label": { "zh": "New API", "en": "New API" },
  "icon": "icon.png",
  "protocol_version": 2,
  "runtime": "go",
  "android": {
    "format": "cph-native-v1",
    "library": "lib/arm64-v8a/libcphplugin.so",
    "abi": "arm64-v8a",
    "min_sdk": 24,
    "files": {
      "lib/arm64-v8a/libcphplugin.so": "<library-sha256>",
      "icon.png": "<icon-sha256>"
    }
  }
}
```

Android 包另含 `signature.json`，以 `SHA256withRSA` 签名原始清单字节，携带公开证书和签名。`android.files` 为库和资源的摘要，签名不放在 manifest 内。主 APP 验证签名者是否与自身发行证书一致。

Lua 包与平台无关，包含脚本、清单和可选图标：

```json
{
  "name": "autoclaw",
  "version": "0.1.3",
  "author": "cph",
  "protocol_version": 2,
  "runtime": "lua",
  "entry": "main.lua",
  "label": { "zh": "AutoClaw", "en": "AutoClaw" },
  "icon": "icon.png"
}
```

Lua 包不携带 Lua Host 二进制，使用宿主安装并启用的运行时。业务能力、登录方式和字段 schema 通过运行时 Handshake 返回，不写入上述包清单。

## 发布清单

每个 `<name>-v<version>` Release 提供独立 `manifest.json`。以下列出 Windows 和 Android；Go 发布清单还包含 `linux-amd64`、`linux-arm64`、`darwin-amd64`、`darwin-arm64`，字段与 Windows 相同。

```json
{
  "schema_version": 1,
  "name": "newapi",
  "version": "0.1.6",
  "runtime": "go",
  "protocol_version": 2,
  "artifacts": {
    "windows-amd64": {
      "download_url": "https://github.com/ShadowSmallBaby/ClawProxyHubPlugins/releases/download/newapi-v0.1.6/newapi-0.1.6-windows-amd64.cphplugin",
      "sha256": "<windows-package-sha256>",
      "size": 5600000,
      "format": "cph-go-v1"
    },
    "android-arm64": {
      "download_url": "https://github.com/ShadowSmallBaby/ClawProxyHubPlugins/releases/download/newapi-v0.1.6/newapi-0.1.6-android-arm64.cphplugin",
      "sha256": "<android-package-sha256>",
      "size": 4600000,
      "format": "cph-native-v1",
      "min_sdk": 24
    }
  }
}
```

`size` 为包文件字节数，`sha256` 为整个包的摘要。Lua 的 `runtime` 为 `lua`，只有 `artifacts.any`，格式为 `cph-lua-v1`，URL 指向平台无关脚本包。Android CI 发布 arm64，x86_64 供本地模拟器构建使用。

## 索引条目

`index.json` 顶层是数组，Go 插件的单个条目如下：

```json
{
  "name": "newapi",
  "version": "0.1.6",
  "author": "cph",
  "label": { "zh": "New API", "en": "New API" },
  "published_at": "2026-10-08",
  "download_url": "https://github.com/ShadowSmallBaby/ClawProxyHubPlugins/releases/download/newapi-v0.1.6/newapi-0.1.6.cphplugin",
  "sha256": "<desktop-compatibility-package-sha256>",
  "platforms": {
    "windows": ["amd64"],
    "linux": ["amd64", "arm64"],
    "darwin": ["amd64", "arm64"],
    "android": ["arm64-v8a"]
  },
  "protocol_version": 2,
  "release_manifest": {
    "download_url": "https://github.com/ShadowSmallBaby/ClawProxyHubPlugins/releases/download/newapi-v0.1.6/manifest.json",
    "sha256": "<release-manifest-sha256>"
  }
}
```

顶层 `download_url` / `sha256` 指向 Go 桌面兼容包或 Lua 通用脚本包。Go 条目的 `platforms` 按操作系统列出支持的架构，Android 使用标准 ABI 名称；空数组或缺少该平台字段表示不支持，不使用布尔值或通配符。后续平台可增加 `ios` 等键。Lua 条目显式带 `runtime: "lua"`，不声明原生架构；能否使用取决于设备上的 Lua Host 是否已安装并启用。

Android 市场对 Go 插件使用 `platforms.android` 初筛，安装时读取发布清单中的 `android-arm64`，再检查最低系统版本、格式、摘要和大小。Lua 直接下载条目中的通用脚本包，校验摘要、包内身份和协议后交给核心安装，不需要单独发布 Android 包。索引不重复保存各平台的包下载信息。

桌面宿主及 Android Go 插件安装先验证 `release_manifest` 的摘要，再选择平台包；清单校验失败时拒绝安装。桌面仍兼容没有 `release_manifest` 的旧索引。Lua 的发布清单保留 `artifacts.any`，供桌面安装和发布工具使用。

## 生成流程

1. `go run ./tools/pack -only newapi,autoclaw` 生成桌面或 Lua `.cphplugin` 和 `build`；此时索引只包含基础条目。
2. [Android 构建工程](android/README.md)生成签名原生包，`verify_packages.py` 校验签名、文件摘要和 ELF。
3. `tools/android_release.py` 收集实际 Android 产物，`tools/release_manifest.py` 拆分平台包、生成发布清单，补充 `protocol_version`、`platforms` 和 `release_manifest`。
4. CI 校验或上传全部不可变资产后，才回写唯一的 `index.json`。索引由 CI 维护，不手工为未上传的包填写下载地址。
