# Android 原生插件构建

本目录是插件子库的独立 Gradle 工程，使用 JDK、Go 和 NDK 生成签名 `.cphplugin`。不加载主 APP、AGP、Compose 或 Web 工程；核心依赖仅来自子库 `go.mod` 中的 SDK。宿主 APP、Lua Host、`.cphhost` 和设备端加载器由核心仓库维护。

## 环境与 SDK

- JDK 21、`go.mod` 指定的 Go 版本、Gradle wrapper 9.7.1。
- Android NDK `28.2.13676358`，通过 `ANDROID_HOME` 或 `ANDROID_SDK_ROOT` 定位；无需安装 Android platform 或 build-tools。
- SDK 必须包含 `sdk/androidplugin`，正常构建关闭 Go workspace 并只读使用锁定依赖。

升级 SDK 时，先推送包含 `sdk/androidplugin` 的核心提交或发布 tag，再执行以下命令，将 `<tag-or-commit>` 替换为实际远程引用，并提交更新后的 `go.mod` / `go.sum`：

```sh
go get "github.com/ShadowSmallBaby/ClawProxyHub@<tag-or-commit>"
go mod tidy
```

核心应用 v2.0.0 的 Go module 路径仍不含 `/v2`，依赖该提交时使用 Go 自动解析的 pseudo-version；不要直接填写 `v2.0.0` 或修改 SDK import 路径。

SDK 升级时按 [AGENTS.md](../AGENTS.md) 同步已发布插件版本。`checkAndroidSdk` 在编译前检查锁定依赖是否包含所需接口。

## 本地打包

从插件仓库根目录执行：

```sh
sh android/gradlew -p android -PcphPlugins=newapi -PcphABI=arm64-v8a packageCphPlugin
python3 android/verify_packages.py build/android/plugin-packages --output build/android/verification.json
```

Windows 使用 `./android/gradlew.bat`。`cphPlugins` 支持逗号分隔名称或 `all`，默认 `all`；`cphABI` 默认 `arm64-v8a`，本地模拟器可用 `x86_64`。CI 仅构建 arm64-v8a。

生成入口和 Go 共享库位于 `build/android/business-projects/<name>/`，包位于 `build/android/plugin-packages/<name>-<version>-android-<arch>.cphplugin`。入口调用业务包的 `New(version)`，版本来自插件 `manifest.json`。`verify_packages.py` 检查签名、文件摘要、ABI 与 ELF 16 KiB 对齐，可通过 `--certificate-sha256` 固定预期发行证书。

## 签名与发布

本地 debug 使用 Android 标准 `~/.android/debug.keystore`；设置 `ANDROID_USER_HOME` 时从该目录读取，缺失时生成 RSA 调试密钥。同一机器上的 debug APP 与插件默认使用同一证书。

正式构建增加 `-PcphReleaseSigning=true`，必须显式设置：

- `CPH_ANDROID_KEYSTORE`：keystore 路径。
- `CPH_ANDROID_STORE_PASSWORD`、`CPH_ANDROID_KEY_ALIAS`、`CPH_ANDROID_KEY_PASSWORD`。

这些变量由插件构建器直接读取。主 APP 仅信任与自身签名相同的插件，所以两个仓库的正式发行必须使用同一套 RSA 证书。缺失配置或使用非 RSA 密钥时拒绝正式打包。

GitHub Actions 在两个仓库配置相同的 `CPH_ANDROID_KEYSTORE_BASE64` 及上述后三项 Secrets。本库 `prepare_signing.py` 只在 runner 临时目录还原密钥，导出 `CPH_ANDROID_KEYSTORE` 与证书摘要，工作流结束后删除临时文件。PR 使用 debug 签名，仅上传检查产物。

仅目标为 main 的 PR 和 main 推送自动运行 CI；手动运行也仅 main 执行构建，选择其他分支时任务跳过。目标为 main 的 PR 始终执行 Go 静态检查、单元测试和发布脚本测试；main 自动发布跳过这组重复检测，手动运行可用 `run_checks=true` 开启。SDK 检查、构建与包验签始终执行。

手动运行 `build` 默认 `publish=false`，使用正式签名构建和验证，将包上传到 Actions 附件。合入 `main` 会自动发布；手动补发需选择 `main` 并设置 `publish=true`。发布使用工作流声明的 `contents: write` 权限，分支规则还须允许该工作流回写 `index.json`。

`build.yml` 从本库构建、验证原生包；`tools/android_release.py` 负责复用不可变资产、发布和更新唯一的 `index.json`。包格式、资产名称与已有下载地址保持一致。设备验收使用核心仓库的测试 APK 和 `android/smoke_plugins.py`，见[宿主验证说明](https://github.com/ShadowSmallBaby/ClawProxyHub/blob/develop/android/README.md)。

## 文件职责

| 文件 | 职责 |
| --- | --- |
| `settings.gradle` / `business-plugin.gradle` | 选择插件、生成入口、组装 `.cphplugin` |
| `plugin-template/` | 共用 Go/JNI 入口 |
| `native.gradle` | 子库 Go module 的 Android ABI 编译 |
| `signing.gradle` / `prepare_signing.py` | 独立读取签名配置和准备 CI 密钥 |
| `verify_packages.py` / `PluginSignature.java` | 离线包和证书验证 |
