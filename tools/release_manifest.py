"""从不可变的桌面兼容包生成平台包；发布清单单独列出 Android 原生包。"""
import hashlib
from copy import copy
import json
from pathlib import Path
import zipfile

PLATFORMS = ('windows-amd64', 'linux-amd64', 'linux-arm64', 'darwin-amd64', 'darwin-arm64')
ANDROID_ARCHES = {'arm64-v8a': 'arm64'}


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def prepare(packages, item, repo, download, android):
    name, version = item['name'], item['version']
    tag = f'{name}-v{version}'
    legacy = packages / f'{name}-{version}.cphplugin'
    if not legacy.exists():
        download(repo, tag, legacy.name, packages)
    if digest(legacy) != item['sha256']:
        raise ValueError(f'{name}: legacy package checksum mismatch')
    base = f'https://github.com/{repo}/releases/download/{tag}/'
    artifacts = {}
    platforms = {}
    uploads = [legacy]
    with zipfile.ZipFile(legacy) as archive:
        manifest = json.loads(archive.read('manifest.json'))
        if manifest['name'] != name or manifest['version'] != version or manifest.get('android'):
            raise ValueError('Legacy package identity mismatch or contains Android content')
        lua = manifest.get('runtime') == 'lua'
        if lua != (item.get('runtime') == 'lua'):
            raise ValueError('Legacy runtime differs from catalog')
        binaries = {f'plugin-{platform}' + ('.exe' if platform.startswith('windows-') else '') for platform in PLATFORMS}
        names = set(archive.namelist())
        if len(names) != len(archive.infolist()) or any('android' in n or (n.startswith('plugin-') and n not in binaries) for n in names):
            raise ValueError('Unexpected platform content in legacy package')
        if lua:
            artifacts['any'] = {'download_url': item['download_url'], 'sha256': item['sha256'], 'size': legacy.stat().st_size, 'format': 'cph-lua-v1'}
        else:
            for platform in PLATFORMS:
                system, arch = platform.split('-')
                platforms.setdefault(system, []).append(arch)
                binary = f'plugin-{platform}' + ('.exe' if platform.startswith('windows-') else '')
                if binary not in names:
                    raise ValueError(f'Missing legacy binary: {binary}')
                path = packages / f'{name}-{version}-{platform}.cphplugin'
                with zipfile.ZipFile(path, 'w', compression=zipfile.ZIP_DEFLATED, compresslevel=9) as output:
                    for info in sorted(archive.infolist(), key=lambda entry: entry.filename):
                        if info.filename not in binaries or info.filename == binary:
                            output.writestr(copy(info), archive.read(info.filename))
                artifacts[platform] = {'download_url': base + path.name, 'sha256': digest(path), 'size': path.stat().st_size, 'format': 'cph-go-v1'}
                uploads.append(path)
    for abi, artifact in android.items():
        if abi not in ANDROID_ARCHES:
            raise ValueError('Unsupported Android publication ABI')
        platform = 'android-' + ANDROID_ARCHES[abi]
        artifacts[platform] = dict(artifact)
        path = packages / f'{name}-{version}-{platform}.cphplugin'
        artifacts[platform]['size'] = path.stat().st_size
        uploads.append(path)
    # 原生包声明架构，Lua 脚本的设备支持由宿主运行时决定。
    if not lua:
        platforms['android'] = sorted(android)
    release = {'schema_version': 1, 'name': name, 'version': version, 'runtime': 'lua' if lua else 'go',
               'protocol_version': manifest['protocol_version'], 'artifacts': artifacts}
    directory = packages / f'{name}-{version}'
    directory.mkdir(exist_ok=True)
    path = directory / 'manifest.json'
    path.write_text(json.dumps(release, ensure_ascii=False, sort_keys=True, indent=2) + '\n', encoding='utf-8')
    uploads.append(path)
    item['protocol_version'] = release['protocol_version']
    item['release_manifest'] = {'download_url': base + 'manifest.json', 'sha256': digest(path)}
    if lua:
        item.pop('platforms', None)
    else:
        item['platforms'] = platforms
    item.pop('android', None)
    return uploads
