"""从 CI Secrets 准备临时签名文件；主 APP 与官方插件复用同一证书。"""
import base64
import hashlib
import os
from pathlib import Path
import subprocess


def main():
    required = ('CPH_ANDROID_KEYSTORE_BASE64', 'CPH_ANDROID_STORE_PASSWORD',
                'CPH_ANDROID_KEY_ALIAS', 'CPH_ANDROID_KEY_PASSWORD', 'RUNNER_TEMP', 'GITHUB_ENV')
    for name in required:
        if not os.environ.get(name):
            raise SystemExit(f'Missing release signing configuration: {name}')
    path = Path(os.environ['RUNNER_TEMP']) / 'cph-release-signing.jks'
    path.write_bytes(base64.b64decode(os.environ['CPH_ANDROID_KEYSTORE_BASE64'], validate=True))
    path.chmod(0o600)
    suffix = '.exe' if os.name == 'nt' else ''
    keytool = Path(os.environ['JAVA_HOME']) / 'bin' / ('keytool' + suffix)
    result = subprocess.run([str(keytool), '-exportcert', '-keystore', str(path),
                             '-storepass:env', 'CPH_ANDROID_STORE_PASSWORD',
                             '-alias', os.environ['CPH_ANDROID_KEY_ALIAS']], capture_output=True, check=True)
    fingerprint = hashlib.sha256(result.stdout).hexdigest()
    with open(os.environ['GITHUB_ENV'], 'a', encoding='utf-8') as output:
        output.write(f'CPH_ANDROID_KEYSTORE={path}\nCPH_ANDROID_CERTIFICATE_SHA256={fingerprint}\n')


if __name__ == '__main__':
    main()
