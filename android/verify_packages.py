"""核对宿主管理的原生插件包、文件签名、ABI 与 ELF 16 KiB 对齐。"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import struct
import subprocess
import tempfile
import zipfile


def verify(path, java, classes, temporary):
    with zipfile.ZipFile(path) as package:
        names = [entry.filename for entry in package.infolist() if not entry.is_dir()]
        if len(names) != len(set(n.lower() for n in names)) or any(
                not n or n.startswith('/') or '\\' in n or ':' in n or '..' in PurePosixPath(n).parts for n in names):
            raise ValueError('unsafe or duplicate package path')
        if sum(e.file_size for e in package.infolist()) > 128 * 1024 * 1024:
            raise ValueError('package exceeds extraction limit')
        raw = package.read('manifest.json')
        manifest = json.loads(raw)
        android = manifest['android']
        signature = json.loads(package.read('signature.json'))
        if android['format'] != 'cph-native-v1' or signature['algorithm'] != 'SHA256withRSA':
            raise ValueError('unsupported native package contract')
        if set(names) != set(android['files']) | {'manifest.json', 'signature.json'}:
            raise ValueError('unsigned package entries')
        for name, digest in android['files'].items():
            if hashlib.sha256(package.read(name)).hexdigest() != digest:
                raise ValueError('modified package content')
        library = f"lib/{android['abi']}/libcphplugin.so"
        if android['library'] != library or any(n.endswith('.apk') for n in names):
            raise ValueError('expected a native library package, without APKs')
        elf = package.read(library)
    machine = {'arm64-v8a': 183, 'x86_64': 62}[android['abi']]
    if elf[:6] != b'\x7fELF\x02\x01' or struct.unpack_from('<H', elf, 18)[0] != machine:
        raise ValueError('native ELF64 machine differs from ABI')
    offset = struct.unpack_from('<Q', elf, 32)[0]
    size, count = struct.unpack_from('<HH', elf, 54)
    loads = 0
    for i in range(count):
        kind, _, file_offset, address, _, _, _, alignment = struct.unpack_from('<IIQQQQQQ', elf, offset + size * i)
        if kind == 1:
            loads += 1
            if alignment < 16384 or file_offset % 16384 != address % 16384:
                raise ValueError('native LOAD segment is not 16 KiB aligned')
    if not loads:
        raise ValueError('missing LOAD segments')
    manifest_file, signature_file, cert_file = [temporary / name for name in ['manifest.json', 'signature.bin', 'certificate.der']]
    manifest_file.write_bytes(raw)
    signature_file.write_bytes(base64.b64decode(signature['value'], validate=True))
    cert_file.write_bytes(base64.b64decode(signature['certificate'], validate=True))
    result = subprocess.run([str(java), '-cp', str(classes), 'PluginSignature', str(manifest_file), str(signature_file), str(cert_file)], check=True, capture_output=True, text=True)
    return {'plugin': manifest['name'], 'version': manifest['version'], 'abi': android['abi'],
            'package_bytes': path.stat().st_size, 'native_bytes': len(elf), 'certificate_sha256': result.stdout.strip()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('packages', type=Path)
    parser.add_argument('--java-home', type=Path, default=Path(os.environ.get('JAVA_HOME', '')))
    parser.add_argument('--certificate-sha256')
    parser.add_argument('--output', type=Path)
    args = parser.parse_args()
    suffix = '.exe' if os.name == 'nt' else ''
    packages = sorted(args.packages.glob('*-android-*.cphplugin'))
    if not packages:
        parser.error('no native plugin packages found')
    with tempfile.TemporaryDirectory(prefix='cph-plugin-verify-') as directory:
        temp = Path(directory)
        subprocess.run([str(args.java_home / 'bin' / ('javac' + suffix)), '-encoding', 'UTF-8', '-d', str(temp), str(Path(__file__).with_name('PluginSignature.java'))], check=True)
        reports = [verify(path, args.java_home / 'bin' / ('java' + suffix), temp, temp) for path in packages]
    certificates = {r['certificate_sha256'] for r in reports}
    if len(certificates) != 1 or args.certificate_sha256 and certificates != {args.certificate_sha256.lower()}:
        raise ValueError('plugin publisher certificate mismatch')
    if args.output:
        args.output.write_text(json.dumps(reports, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(f'Verified {len(reports)} signed native packages; certificate {next(iter(certificates))}')


if __name__ == '__main__':
    main()
