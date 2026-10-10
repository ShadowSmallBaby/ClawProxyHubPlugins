"""Android 插件分架构补发；已有同名资产保持不变，索引只引用验证过的实际包。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import zipfile
from release_manifest import ANDROID_ARCHES as ARCHES, prepare


def gh(*args):
    return subprocess.run(['gh', *args], check=True, capture_output=True, text=True).stdout


def assets(repo, tag):
    result = subprocess.run(['gh', 'api', f'repos/{repo}/releases/tags/{tag}'], capture_output=True, text=True)
    if result.returncode:
        if 'HTTP 404' in result.stderr:
            return None
        raise RuntimeError(f'Cannot inspect release {tag}: {result.stderr}')
    return {item['name']: item for item in json.loads(result.stdout)['assets']}


def download(repo, tag, filename, directory):
    gh('release', 'download', tag, '--repo', repo, '--pattern', filename, '--dir', str(directory), '--clobber')


def plan(repo, abi, out):
    out.mkdir(parents=True, exist_ok=True)
    missing = []
    for path in sorted(Path('plugins-go').glob('*/manifest.json')):
        manifest = json.loads(path.read_text(encoding='utf-8'))
        name, version = manifest['name'], manifest['version']
        validate_identity(name, version)
        filename = f'{name}-{version}-android-{ARCHES[abi]}.cphplugin'
        tag = f'{name}-v{version}'
        released = assets(repo, tag)
        if released is not None and filename in released:
            download(repo, tag, filename, out)
        else:
            missing.append(name)
    return ','.join(missing)


def validate_identity(name, version):
    if not re.fullmatch(r'[a-z][a-z0-9_]{0,63}', name) or not re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', version):
        raise ValueError('Invalid plugin name or version')


def collect_android(packages, catalog, repo):
    base = {item['name']: item for item in catalog if item.get('runtime', 'go') != 'lua'}
    result = {}
    for path in sorted(packages.glob('*-android-*.cphplugin')):
        with zipfile.ZipFile(path) as archive:
            manifest = json.loads(archive.read('manifest.json'))
        name, version = manifest['name'], manifest['version']
        validate_identity(name, version)
        android = manifest['android']
        abi = android['abi']
        if (abi not in ARCHES or android['format'] != 'cph-native-v1' or manifest.get('runtime') != 'go'
                or manifest['protocol_version'] != 2 or not isinstance(android['min_sdk'], int) or android['min_sdk'] < 24):
            raise ValueError('Invalid Android package contract')
        if name not in base or base[name]['version'] != version:
            raise ValueError(f'{name}: package version differs from published desktop catalog')
        if path.name != f'{name}-{version}-android-{ARCHES[abi]}.cphplugin':
            raise ValueError('Package filename differs from identity')
        platforms = result.setdefault(name, {})
        if abi in platforms:
            raise ValueError('Duplicate Android ABI')
        with path.open('rb') as stream:
            digest = hashlib.file_digest(stream, 'sha256').hexdigest()
        platforms[abi] = {'format': android['format'], 'min_sdk': android['min_sdk'],
                          'download_url': f'https://github.com/{repo}/releases/download/{name}-v{version}/{path.name}',
                          'sha256': digest}
    if set(result) != set(base) or any(set(platforms) != set(ARCHES) for platforms in result.values()):
        raise ValueError('Android publication requires arm64-v8a for every Go plugin in the catalog')
    return result


def publish(repo, packages, catalog):
    android = collect_android(packages, catalog, repo)
    index = [dict(item) for item in catalog]
    for item in index:
        name, version = item['name'], item['version']
        tag = f'{name}-v{version}'
        released = assets(repo, tag)
        uploads = prepare(packages, item, repo, download, android.get(name, {}))
        if released is None:
            path = packages / f'{name}-{version}.cphplugin'
            with path.open('rb') as stream:
                if hashlib.file_digest(stream, 'sha256').hexdigest() != item['sha256']:
                    raise ValueError('Desktop package checksum mismatch')
            gh('release', 'create', tag, str(path), '--repo', repo, '--target', os.environ['GITHUB_SHA'],
               '--title', f'{name} v{version}', '--notes', f"sha256: `{item['sha256']}`")
            released = assets(repo, tag)
        for path in uploads:
            if path.name in released:
                with tempfile.TemporaryDirectory(prefix='cph-released-') as directory:
                    download(repo, tag, path.name, Path(directory))
                    if (Path(directory) / path.name).read_bytes() != path.read_bytes():
                        raise ValueError(f'Refusing to overwrite immutable release asset: {path.name}')
            else:
                gh('release', 'upload', tag, str(path), '--repo', repo)
    # 所有上传成功后才生成待回写索引；失败重试会校验已存在的资产。
    (packages / 'index.json').write_text(json.dumps(index, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['plan', 'publish'])
    parser.add_argument('--repo', required=True)
    parser.add_argument('--packages', type=Path, required=True)
    parser.add_argument('--abi', choices=ARCHES)
    parser.add_argument('--index', type=Path)
    args = parser.parse_args()
    if args.command == 'plan':
        if not args.abi:
            parser.error('--abi required for plan')
        names = plan(args.repo, args.abi, args.packages)
        with open(os.environ['GITHUB_OUTPUT'], 'a', encoding='utf-8') as output:
            output.write(f'names={names}\n')
    else:
        if not args.index:
            parser.error('--index required for publish')
        publish(args.repo, args.packages, json.loads(args.index.read_text(encoding='utf-8')))


if __name__ == '__main__':
    main()
