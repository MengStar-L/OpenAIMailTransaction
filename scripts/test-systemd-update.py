#!/usr/bin/env python3
"""CI-only end-to-end self-update against an already published GitHub release.

This script uses the real Linux installer and systemd, not a systemctl/curl
shim. It owns one disposable /opt prefix and refuses pre-existing installations.
No resource is purchased: the test provider points to loopback port 9 and the
key stored through the settings API is a deliberately fake fixture.
"""
import argparse
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request

try:
    import pwd
except ImportError:
    pwd = None  # Allow syntax/help checks on Windows; execution requires Linux.


PREFIX = Path('/opt/shiguang-update-ci')
UNIT = Path('/etc/systemd/system/shiguang.service')
REPOSITORY = 'MengStar-L/OpenAIMailTransaction'
EXECUTABLE = PREFIX / 'bin/shiguang'
DATA = PREFIX / 'data/live'
INSTALLER = Path(__file__).resolve().with_name('install.sh')
FAKE_KEY = 'ci-only-systemd-update-unused-key'
EXPECTED_EXEC = 'ExecStart=' + str(EXECUTABLE)
WRITABLE_SETTINGS = {
    'brand', 'phone_enabled', 'email_enabled', 'phone_service', 'phone_country',
    'phone_max_price', 'phone_ttl_minutes', 'email_service', 'email_domain',
    'email_max_price', 'email_ttl_minutes', 'background_type', 'background_url',
}


def command(*args, check=True, capture=False):
    return subprocess.run(args, check=check, text=True, stdout=subprocess.PIPE if capture else None)


def systemd_value(name):
    return command('systemctl', 'show', 'shiguang', '-p', name, '--value', capture=True).stdout.strip()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def file_digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


class API:
    def __init__(self, base):
        self.base = base
        self.client = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
        )

    def call(self, path, body=None, method=None, timeout=30):
        assert path.startswith('/api/') or path == '/healthz'
        headers = {'Origin': self.base, 'Accept': 'application/json'}
        payload = None
        if body is not None:
            payload = json.dumps(body).encode()
            headers['Content-Type'] = 'application/json'
        request = urllib.request.Request(self.base + path, data=payload, method=method, headers=headers)
        with self.client.open(request, timeout=timeout) as response:
            data = response.read()
            return data.decode() if path == '/healthz' else json.loads(data)


def wait_health(api, timeout=35):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if api.call('/healthz', timeout=2) == 'ok':
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.25)
    raise AssertionError('Isolated systemd service did not become healthy')


def state_fingerprint():
    database = DATA / 'atelier.db'
    assert database.resolve().is_relative_to(PREFIX)
    with sqlite3.connect(database.as_uri() + '?mode=ro', uri=True) as db:
        db.execute('BEGIN')
        tables = [row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
        result = {}
        for table in tables:
            quoted = '"' + table.replace('"', '""') + '"'
            rows = [[{'blob': value.hex()} if isinstance(value, bytes) else value for value in row]
                    for row in db.execute('SELECT * FROM ' + quoted)]
            rows.sort(key=lambda row: json.dumps(row, sort_keys=True))
            result[table] = {'rows': len(rows), 'sha256': digest(json.dumps(rows, sort_keys=True).encode())}
        assert db.execute('SELECT count(*) FROM orders').fetchone()[0] == 0, 'This acceptance must never allocate resources'
    return {
        'tables': result,
        'env': file_digest(PREFIX / '.env'),
        'secret': file_digest(DATA / 'secret.key'),
        'preferences': file_digest(DATA / 'update-settings.json'),
    }


def verify_service_identity():
    assert systemd_value('User') == 'shiguang'
    assert systemd_value('Group') == 'shiguang'
    assert systemd_value('Restart') == 'always'
    assert systemd_value('WorkingDirectory') == str(PREFIX)
    pid = int(systemd_value('MainPID'))
    assert pid > 1
    uid = Path(f'/proc/{pid}').stat().st_uid
    assert uid != 0 and pwd.getpwuid(uid).pw_name == 'shiguang', 'Service must run as the installation user'
    assert Path(f'/proc/{pid}/exe').resolve() == EXECUTABLE
    command('systemctl', 'is-active', '--quiet', 'shiguang')
    return pid


def initialize(api, password):
    assert api.call('/api/admin/bootstrap')['setup_required'] is True
    api.call('/api/admin/setup', {'password': password, 'confirm_password': password})
    preferences = api.call('/api/admin/updates/settings', {'auto_check': False, 'auto_update': False}, 'PUT')
    assert preferences['settings'] == {'auto_check': False, 'auto_update': False}
    settings = api.call('/api/admin/settings')
    assert settings['mode'] == 'live' and settings['email_ttl_minutes'] == 25
    settings = {key: value for key, value in settings.items() if key in WRITABLE_SETTINGS}
    settings.update(brand='CI systemd update', email_max_price='0.02', api_key=FAKE_KEY)
    saved = api.call('/api/admin/settings', settings, 'PUT')
    assert saved['api_configured'] is True and 'api_key' not in saved
    created = api.call('/api/admin/cdks', {
        'kind': 'email', 'quantity': 3, 'note': 'systemd update persistence acceptance',
        'expires_days': 7, 'usage_limit': 3,
    })
    assert len(created['codes']) == 3
    return sorted(created['codes'])


def verify_data_api(api, password, expected_codes):
    api.call('/api/admin/login', {'password': password})
    settings = api.call('/api/admin/settings')
    assert settings['brand'] == 'CI systemd update'
    assert settings['email_max_price'] == '0.02' and settings['email_ttl_minutes'] == 25
    assert settings['api_configured'] is True
    cdks = api.call('/api/admin/cdks')['items']
    assert sorted(item['code'] for item in cdks) == expected_codes, 'Encrypted CDKs must still decrypt to the same full codes'
    assert all(item['usage_limit'] == 3 and item['status'] == 'available' for item in cdks)


def wait_upgrade(api, password, target_version, old_pid, timeout=480):
    deadline = time.monotonic() + timeout
    phases = []
    session_invalidated = False
    while time.monotonic() < deadline:
        try:
            status = api.call('/api/admin/updates', timeout=4)
            phase = status['phase']
            if not phases or phases[-1] != phase:
                phases.append(phase)
                print('Update phase: ' + phase, flush=True)
            if status['current_version'] == target_version:
                assert verify_service_identity() != old_pid, 'systemd must have started a new process'
                assert session_invalidated, 'The old administrator session should be invalidated by restart'
                return status, phases
            if phase in ('error', 'deferred'):
                raise AssertionError('Update rejected: ' + status.get('error', ''))
        except urllib.error.HTTPError as error:
            if error.code != 401:
                raise
            session_invalidated = True
            api.call('/api/admin/login', {'password': password}, timeout=5)
        except (OSError, urllib.error.URLError):
            pass  # A short connection refusal is expected while systemd restarts.
        time.sleep(1)
    raise AssertionError('Systemd did not restart into the requested release before the deadline')


def safe_cleanup(owned):
    if not owned:
        return
    if UNIT.exists() or UNIT.is_symlink():
        assert not UNIT.is_symlink() and EXPECTED_EXEC in UNIT.read_text().splitlines(), 'Cleanup refused unrelated unit'
        command('systemctl', 'stop', 'shiguang', check=False)
        command('systemctl', 'disable', 'shiguang', check=False)
        UNIT.unlink()
        command('systemctl', 'daemon-reload', check=False)
        command('systemctl', 'reset-failed', 'shiguang', check=False)
    if PREFIX.exists() or PREFIX.is_symlink():
        assert not PREFIX.is_symlink() and PREFIX.resolve() == Path('/opt/shiguang-update-ci'), 'Cleanup refused unexpected directory'
        shutil.rmtree(PREFIX)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--release', default='v1.0.2')
    parser.add_argument('--old-binary', type=Path, required=True)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    assert os.environ.get('GITHUB_ACTIONS') == 'true' and os.environ.get('RUNNER_OS') == 'Linux', 'Disposable GitHub Ubuntu runner required'
    assert sys.platform == 'linux' and os.geteuid() == 0 and Path('/run/systemd/system').is_dir()
    assert re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', args.release), 'A published stable release tag is required'
    assert not PREFIX.exists() and not PREFIX.is_symlink() and not UNIT.exists() and not UNIT.is_symlink(), 'Refusing a pre-existing installation'
    assert not INSTALLER.is_symlink() and INSTALLER.is_file()
    old_binary = args.old_binary.resolve(strict=True)
    assert old_binary.is_file() and not args.old_binary.is_symlink()
    assert 'shiguang 0.9.0 ' in command(str(old_binary), '--version', capture=True).stdout
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
    api = API(f'http://127.0.0.1:{port}')
    owned = False
    report = {'target_release': args.release, 'source_version': '0.9.0', 'success': False}
    try:
        owned = True
        command('bash', str(INSTALLER), '--version', args.release, '--dir', str(PREFIX), '--listen', f'127.0.0.1:{port}', '--no-auto-update')
        command('systemctl', 'stop', 'shiguang')
        # Fence even an accidental provider operation to a closed loopback port.
        config = PREFIX / '.env'
        text = config.read_text()
        assert 'SMSBOWER_API_BASE=https://smsbower.page' in text
        text = text.replace('SMSBOWER_API_BASE=https://smsbower.page', 'SMSBOWER_API_BASE=http://127.0.0.1:9')
        config.write_text(text + '\n# ci-systemd-self-update-preserve\n')
        (DATA / 'update-settings.json').write_text('{"auto_check":false,"auto_update":false}\n')
        command('systemctl', 'start', 'shiguang')
        wait_health(api)
        password = secrets.token_urlsafe(24)
        expected_codes = initialize(api, password)
        assert api.call('/api/admin/updates')['current_version'] == args.release[1:]
        verify_service_identity()
        expected_state = state_fingerprint()
        released_binary_digest = file_digest(EXECUTABLE)

        command('systemctl', 'stop', 'shiguang')
        command('install', '-o', 'shiguang', '-g', 'shiguang', '-m', '0755', str(old_binary), str(EXECUTABLE))
        old_binary_digest = file_digest(EXECUTABLE)
        command('systemctl', 'start', 'shiguang')
        wait_health(api)
        verify_data_api(api, password, expected_codes)
        old_pid = verify_service_identity()
        before = api.call('/api/admin/updates')
        assert before['current_version'] == '0.9.0' and before['supported'] is True
        assert before['settings'] == {'auto_check': False, 'auto_update': False}
        assert state_fingerprint() == expected_state
        checked = api.call('/api/admin/updates/check', {}, timeout=40)
        assert checked['repository'] == REPOSITORY and checked['latest_version'] == args.release and checked['available'] is True
        accepted = api.call('/api/admin/updates/apply', {})
        assert accepted['phase'] == 'downloading'
        updated, phases = wait_upgrade(api, password, args.release[1:], old_pid)
        wait_health(api)
        verify_data_api(api, password, expected_codes)
        assert state_fingerprint() == expected_state, 'Database, .env, key or update preferences changed'
        assert file_digest(EXECUTABLE) == released_binary_digest, 'Self-updater must install the exact official binary already verified by install.sh'
        assert file_digest(PREFIX / 'bin/shiguang.previous') == old_binary_digest
        assert updated['settings'] == {'auto_check': False, 'auto_update': False}
        assert EXECUTABLE.stat().st_uid == pwd.getpwnam('shiguang').pw_uid
        assert config.stat().st_uid == 0 and config.stat().st_mode & 0o777 == 0o640
        print('Manual Check + Apply verified; starting automatic-update acceptance.', flush=True)

        # Run the same low-version binary again with all fixture data intact.
        # Enabling both preferences must wake the real six-hour update loop;
        # this round never calls the check/apply endpoints.
        command('systemctl', 'stop', 'shiguang')
        command('install', '-o', 'shiguang', '-g', 'shiguang', '-m', '0755', str(old_binary), str(EXECUTABLE))
        command('systemctl', 'start', 'shiguang')
        wait_health(api)
        verify_data_api(api, password, expected_codes)
        auto_old_pid = verify_service_identity()
        assert api.call('/api/admin/updates')['current_version'] == '0.9.0'
        assert state_fingerprint() == expected_state
        enabled = api.call('/api/admin/updates/settings', {'auto_check': True, 'auto_update': True}, 'PUT')
        assert enabled['settings'] == {'auto_check': True, 'auto_update': True}
        automatic_expected = state_fingerprint()
        assert {key: value for key, value in automatic_expected.items() if key != 'preferences'} == {
            key: value for key, value in expected_state.items() if key != 'preferences'
        }
        automatic_updated, automatic_phases = wait_upgrade(api, password, args.release[1:], auto_old_pid)
        verify_data_api(api, password, expected_codes)
        assert state_fingerprint() == automatic_expected
        assert file_digest(EXECUTABLE) == released_binary_digest
        assert file_digest(PREFIX / 'bin/shiguang.previous') == old_binary_digest
        assert automatic_updated['settings'] == {'auto_check': True, 'auto_update': True}
        report.update(success=True, version_after=updated['current_version'], pid_changed=True,
                      non_root_service=True, systemd_restart='always', official_binary_match=True,
                      previous_binary_verified=True, database_and_config_unchanged=True,
                      encrypted_key_and_cdks_preserved=True, preferences_preserved=True,
                      resource_purchases=0, manual_update_verified=True, automatic_update_verified=True,
                      manual_phases=phases, automatic_phases=automatic_phases,
                      table_counts={name: value['rows'] for name, value in expected_state['tables'].items()})
        print(json.dumps(report, ensure_ascii=False), flush=True)
    except BaseException as error:
        report['error'] = str(error)
        command('journalctl', '-u', 'shiguang', '-n', '100', '--no-pager', check=False)
        raise
    finally:
        try:
            safe_cleanup(owned)
            report['isolated_installation_cleaned'] = owned
        finally:
            args.report.parent.mkdir(parents=True, exist_ok=True)
            args.report.write_text(json.dumps(report, indent=2, ensure_ascii=False))


if __name__ == '__main__':
    main()
