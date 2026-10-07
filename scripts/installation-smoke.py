#!/usr/bin/env python3
"""CI-only installer fixtures and checks. Never connects to the resource provider."""
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import re
import shutil
import socket
import sqlite3
import subprocess
import sys
import urllib.request
import urllib.error


assert os.environ.get("GITHUB_ACTIONS") == "true", "Disposable GitHub runner required"
ROOT = Path("/opt/shiguang-ci")
WORK = Path(os.environ["SHIGUANG_SMOKE_DIR"])
assert str(WORK).startswith("/tmp/shiguang-ci-smoke.")
REPO = "MengStar-L/OpenAIMailTransaction"
BASE = os.environ.get("SHIGUANG_SMOKE_BASE", "http://127.0.0.1:8080")
assert BASE in ("http://127.0.0.1:8080", "http://127.0.0.1:18081")
PASSWORD = "ci-only-installation-smoke-password"
FAKE_KEY = "ci-only-unused-provider-key"
COOKIE_JAR = http.cookiejar.CookieJar()
CLIENT = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(COOKIE_JAR), urllib.request.ProxyHandler({}))


def digest(data):
    return hashlib.sha256(data).hexdigest()


def api(path, data=None, method=None):
    body = None if data is None else json.dumps(data).encode()
    request = urllib.request.Request(BASE + path, data=body, method=method,
                                     headers={"Content-Type": "application/json", "Origin": BASE})
    try:
        with CLIENT.open(request, timeout=10) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        # Fixtures contain no real credentials; expose the server's validation
        # message so an API-contract mismatch is actionable in CI.
        detail = error.read(4096).decode(errors="replace")
        raise AssertionError(f"Smoke API {path}: HTTP {error.code}: {detail}") from None


def data_fingerprint():
    database = ROOT / "data/live/atelier.db"
    with sqlite3.connect(database.as_uri() + "?mode=ro", uri=True) as db:
        db.execute("BEGIN")
        tables = [row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
        result = {}
        for table in tables:
            quoted = '"' + table.replace('"', '""') + '"'
            rows = [[{"blob": value.hex()} if isinstance(value, bytes) else value for value in row]
                    for row in db.execute("SELECT * FROM " + quoted)]
            result[table] = sorted(rows, key=lambda row: json.dumps(row, sort_keys=True))
    return digest(json.dumps(result, sort_keys=True, ensure_ascii=False).encode())


def state():
    return {
        "database": data_fingerprint(),
        "config": digest((ROOT / ".env").read_bytes()),
        "secret": digest((ROOT / "data/live/secret.key").read_bytes()),
        "preferences": digest((ROOT / "data/live/update-settings.json").read_bytes()),
    }


def fixtures(archive):
    checksum = digest(archive.read_bytes())
    for version in ("999.0.0", "999.0.1"):
        directory = WORK / ("v" + version)
        directory.mkdir()
        name = f"shiguang_{version}_linux_amd64.tar.gz"
        shutil.copyfile(archive, directory / name)
        (directory / "checksums.txt").write_text(f"{checksum}  {name}\n")


def curl_shim(arguments):
    urls = [argument for argument in arguments if argument.startswith(("https://", "http://"))]
    assert len(urls) == 1, "Unexpected installer curl invocation"
    url = urls[0]
    if any(url.startswith(base + "/") for base in ("http://127.0.0.1:8080", "http://127.0.0.1:18081")):
        os.execv("/usr/bin/curl", ["curl", *arguments])
    if url == f"https://github.com/{REPO}/releases/latest":
        assert "%{url_effective}" in arguments
        print(f"https://github.com/{REPO}/releases/tag/v999.0.0", end="")
        return
    prefix = f"https://github.com/{REPO}/releases/download/"
    assert url.startswith(prefix), "Unexpected external URL in installer smoke"
    relative = url[len(prefix):]
    version, name = relative.split("/")
    assert version in ("v999.0.0", "v999.0.1")
    assert name in ("checksums.txt", f"shiguang_{version[1:]}_linux_amd64.tar.gz")
    output = Path(arguments[arguments.index("--output") + 1])
    assert output.is_absolute() and str(output).startswith("/tmp/shiguang-install.")
    if name == "checksums.txt" and os.environ.get("SHIGUANG_SMOKE_BAD_CHECKSUM") == "1":
        output.write_text(f"{'0' * 64}  shiguang_{version[1:]}_linux_amd64.tar.gz\n")
    else:
        shutil.copyfile(WORK / version / name, output)


def initialize():
    assert_listening("0.0.0.0:8080")
    assert api("/api/admin/bootstrap")["setup_required"] is True
    assert json.loads((ROOT / "data/live/update-settings.json").read_text()) == {"auto_check": True, "auto_update": True}
    api("/api/admin/setup", {"password": PASSWORD, "confirm_password": PASSWORD})
    # Turn off network update lookups in this fixture before the initial 15-second timer.
    updated = api("/api/admin/updates/settings", {"auto_check": False, "auto_update": False}, "PUT")
    assert updated["settings"] == {"auto_check": False, "auto_update": False}
    settings = api("/api/admin/settings")
    assert settings["email_ttl_minutes"] == 25
    assert settings["mode"] == "live"
    assert settings["api_configured"] is False
    writable = {
        "brand", "phone_enabled", "email_enabled", "phone_service",
        "phone_country", "phone_max_price", "phone_ttl_minutes",
        "email_service", "email_domain", "email_max_price", "email_ttl_minutes",
        "background_type", "background_url",
    }
    settings = {key: value for key, value in settings.items() if key in writable}
    settings.update(brand="CI 拾光", email_max_price="0.02", api_key=FAKE_KEY)
    saved = api("/api/admin/settings", settings, "PUT")
    assert saved["api_configured"] is True
    assert "api_key" not in saved
    result = api("/api/admin/cdks", {"kind": "email", "quantity": 2, "note": "installer persistence smoke", "expires_days": 7, "usage_limit": 3})
    assert len(result["codes"]) == 2
    (WORK / "codes.json").write_text(json.dumps(sorted(result["codes"])))
    assert api("/api/admin/bootstrap")["setup_required"] is False


def assert_listening(address):
    host, port = address.rsplit(":", 1)
    expected = socket.inet_aton(host)[::-1].hex().upper() + f":{int(port):04X}"
    listeners = [line.split()[1] for line in Path("/proc/net/tcp").read_text().splitlines()[1:]
                 if line.split()[3] == "0A"]
    assert expected in listeners, f"Service is not listening on {address}"
    assert re.search(r"(?m)^LISTEN_ADDR=" + re.escape(address) + r"$", (ROOT / ".env").read_text())


def unchanged(expected_listen=None):
    expected = json.loads((WORK / "expected.json").read_text())
    if expected_listen is not None:
        previous = (WORK / "expected.env").read_text()
        changed = re.sub(r"(?m)^LISTEN_ADDR=.*$", "LISTEN_ADDR=" + expected_listen, previous)
        assert changed != previous, "Explicit listening-address fixture must actually change the address"
        expected["config"] = digest(changed.encode())
    assert state() == expected, "Existing settings, credentials, database or update preferences changed"


def verify(expected_listen=None, backup_count=1):
    unchanged(expected_listen)
    previous_listen = re.search(r"(?m)^LISTEN_ADDR=(.*)$", (WORK / "expected.env").read_text()).group(1)
    assert_listening(expected_listen or previous_listen)
    assert api("/api/admin/bootstrap")["setup_required"] is False
    api("/api/admin/login", {"password": PASSWORD})
    settings = api("/api/admin/settings")
    assert settings["brand"] == "CI 拾光" and settings["email_ttl_minutes"] == 25
    assert settings["api_configured"] is True
    items = api("/api/admin/cdks")["items"]
    assert sorted(item["code"] for item in items) == json.loads((WORK / "codes.json").read_text())
    assert all(item["usage_limit"] == 3 for item in items)
    backups = sorted((ROOT / "backups").iterdir(), key=lambda path: path.stat().st_mtime_ns)
    assert len(backups) == backup_count, "Expected one backup for each successful upgrade"
    backup = backups[-1]
    assert (backup / "data/live/atelier.db").is_file()
    assert (backup / "data/live/secret.key").read_bytes() == (ROOT / "data/live/secret.key").read_bytes()
    assert (backup / ".env").read_bytes() == (WORK / "expected.env").read_bytes()
    assert (ROOT / "bin/shiguang.previous").is_file()
    assert (ROOT / "bin/shiguang").stat().st_uid != 0
    assert (ROOT / ".env").stat().st_uid == 0
    assert (ROOT / ".env").stat().st_mode & 0o777 == 0o640
    subprocess.run(["systemctl", "is-active", "--quiet", "shiguang"], check=True)


command = sys.argv[1]
if command == "prepare":
    fixtures(Path(sys.argv[2]))
elif command == "curl":
    curl_shim(sys.argv[2:])
elif command == "initialize":
    initialize()
elif command == "capture":
    (WORK / "expected.json").write_text(json.dumps(state()))
    shutil.copyfile(ROOT / ".env", WORK / "expected.env")
elif command == "unchanged":
    unchanged()
elif command == "verify":
    verify(sys.argv[2] if len(sys.argv) > 2 and sys.argv[2] != "preserve" else None,
           int(sys.argv[3]) if len(sys.argv) > 3 else 1)
else:
    raise SystemExit("Unknown smoke phase")
