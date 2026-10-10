import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import android_release as release
from release_manifest import PLATFORMS, digest


class AndroidReleaseTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.catalog = [{'name': 'test', 'version': '1.0.0', 'label': {'en': 'Test'}, 'download_url': 'https://example.com/desktop.cphplugin', 'sha256': 'desktop-sha', 'extra': {'preserved': True}},
                        {'name': 'script', 'version': '1.0.0', 'runtime': 'lua'}]
        for item in self.catalog:
            path = self.root / f"{item['name']}-1.0.0.cphplugin"
            with zipfile.ZipFile(path, 'w') as package:
                package.writestr('manifest.json', json.dumps({'name': item['name'], 'version': '1.0.0', 'runtime': item.get('runtime', 'go'), 'protocol_version': 2}))
                package.writestr('icon.png', b'icon')
                if item.get('runtime') == 'lua':
                    package.writestr('main.lua', b'return {}')
                else:
                    for platform in PLATFORMS:
                        package.writestr('plugin-' + platform + ('.exe' if platform.startswith('windows-') else ''), platform.encode())
            item.update(sha256=digest(path), download_url='https://example.com/' + path.name)
        for abi, arch in release.ARCHES.items():
            with zipfile.ZipFile(self.root / f'test-1.0.0-android-{arch}.cphplugin', 'w') as package:
                package.writestr('manifest.json', json.dumps({'name': 'test', 'version': '1.0.0', 'runtime': 'go', 'protocol_version': 2,
                                  'android': {'format': 'cph-native-v1', 'abi': abi, 'min_sdk': 24}}))

    def test_platform_urls_and_actual_digests(self):
        android = release.collect_android(self.root, self.catalog, 'owner/repo')
        self.assertEqual(set(android), {'test'})
        self.assertEqual(set(android['test']), {'arm64-v8a'})
        self.assertNotIn('android', self.catalog[0])
        for abi, arch in release.ARCHES.items():
            artifact = android['test'][abi]
            filename = f'test-1.0.0-android-{arch}.cphplugin'
            self.assertEqual(artifact['sha256'], hashlib.sha256((self.root / filename).read_bytes()).hexdigest())
            self.assertEqual(artifact['download_url'], f'https://github.com/owner/repo/releases/download/test-v1.0.0/{filename}')

    def test_missing_arm64_never_publishes_partial_index(self):
        (self.root / 'test-1.0.0-android-arm64.cphplugin').unlink()
        with self.assertRaisesRegex(ValueError, 'arm64-v8a'):
            release.collect_android(self.root, self.catalog, 'owner/repo')

    def test_version_mismatch_is_rejected(self):
        self.catalog[0]['version'] = '1.0.1'
        with self.assertRaisesRegex(ValueError, 'version differs'):
            release.collect_android(self.root, self.catalog, 'owner/repo')

    def test_existing_assets_are_immutable_and_index_written_last(self):
        self.catalog[0]['android'] = {'obsolete': {'download_url': 'https://example.com/old'}}
        self.catalog[0]['platforms'] = {'ios': ['arm64']}
        files = {}
        for item in self.catalog:
            name = item['name']
            path = self.root / f'{name}-1.0.0.cphplugin'
            files[f'{name}-v1.0.0'] = {path.name: path.read_bytes()}
        def download(repo, tag, filename, directory):
            (directory / filename).write_bytes(files[tag][filename])
        def upload(*args):
            self.assertEqual(args[:2], ('release', 'upload'))
            path = Path(args[3]); files[args[2]][path.name] = path.read_bytes()
        with patch.object(release, 'assets', side_effect=lambda repo, tag: files[tag]), patch.object(release, 'download', side_effect=download), patch.object(release, 'gh', side_effect=upload) as gh:
            release.publish('owner/repo', self.root, self.catalog)
            self.assertTrue(gh.called)
            gh.reset_mock()
            release.publish('owner/repo', self.root, self.catalog)
            gh.assert_not_called()
            self.assertTrue((self.root / 'index.json').is_file())
            index = json.loads((self.root / 'index.json').read_text())
            self.assertEqual(index[0]['sha256'], self.catalog[0]['sha256'])
            expected_platforms = {'windows': ['amd64'], 'linux': ['amd64', 'arm64'], 'darwin': ['amd64', 'arm64'], 'android': ['arm64-v8a']}
            for item in index:
                self.assertNotIn('android', item)
                if item.get('runtime') == 'lua':
                    self.assertNotIn('platforms', item)
                else:
                    self.assertEqual(item['platforms'], expected_platforms)
                self.assertTrue(item['release_manifest']['download_url'].endswith('/manifest.json'))
            manifest = json.loads(files['test-v1.0.0']['manifest.json'])
            self.assertEqual(set(manifest['artifacts']), set(PLATFORMS) | {'android-arm64'})
            self.assertEqual(set(json.loads(files['script-v1.0.0']['manifest.json'])['artifacts']), {'any'})
            for platform in PLATFORMS:
                with zipfile.ZipFile(self.root / f'test-1.0.0-{platform}.cphplugin') as archive:
                    self.assertEqual(len([n for n in archive.namelist() if n.startswith('plugin-')]), 1)
                    self.assertEqual(archive.read('icon.png'), b'icon')
            with zipfile.ZipFile(self.root / 'test-1.0.0.cphplugin') as archive:
                self.assertFalse(any('android' in name for name in archive.namelist()))
            (self.root / 'index.json').unlink()
            files['test-v1.0.0']['manifest.json'] = b'different published content'
            with self.assertRaisesRegex(ValueError, 'immutable'):
                release.publish('owner/repo', self.root, self.catalog)
            self.assertFalse((self.root / 'index.json').exists())


if __name__ == '__main__':
    unittest.main()
