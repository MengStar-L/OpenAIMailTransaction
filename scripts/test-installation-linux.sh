#!/usr/bin/env bash
# Real systemd installation/upgrade smoke test, restricted to disposable GitHub runners.
set -Eeuo pipefail
[[ "${GITHUB_ACTIONS:-}" == true && "$EUID" == 0 && "$(uname -s)" == Linux ]] || { echo 'Run only as root in a disposable Linux GitHub Actions runner.' >&2; exit 1; }
[[ -d /run/systemd/system ]] || { echo 'No running systemd: cannot validate systemd installation on this runner.' >&2; exit 1; }
(($# == 1)) || { echo 'Usage: test-installation-linux.sh RELEASE_ARCHIVE' >&2; exit 1; }
archive="$(realpath -- "$1")"
[[ -f "$archive" ]] || { echo 'Build archive not found.' >&2; exit 1; }
readonly prefix='/opt/shiguang-ci'
readonly unit='/etc/systemd/system/shiguang.service'
[[ ! -e "$prefix" && ! -L "$prefix" && ! -e "$unit" && ! -L "$unit" ]] || { echo 'Smoke destination already exists; refusing to touch it.' >&2; exit 1; }
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
smoke_dir="$(mktemp -d /tmp/shiguang-ci-smoke.XXXXXXXX)"
export SHIGUANG_SMOKE_DIR="$smoke_dir"
export SHIGUANG_SMOKE_HELPER="$script_dir/installation-smoke.py"

cleanup() {
  result=$?
  trap - EXIT
  if [[ -f "$unit" ]] && grep -Fxq "ExecStart=$prefix/bin/shiguang" "$unit"; then
    if ((result != 0)); then journalctl -u shiguang -n 80 --no-pager || true; fi
    systemctl stop shiguang || true
    systemctl disable shiguang || true
    rm -f -- "$unit"
    systemctl daemon-reload
  fi
  if [[ "$prefix" == /opt/shiguang-ci && ! -L "$prefix" && "$(realpath -m -- "$prefix")" == /opt/shiguang-ci ]]; then
    rm -rf -- "$prefix"
  fi
  if [[ "$smoke_dir" == /tmp/shiguang-ci-smoke.* && ! -L "$smoke_dir" ]]; then rm -rf -- "$smoke_dir"; fi
  exit "$result"
}
trap cleanup EXIT

python3 "$SHIGUANG_SMOKE_HELPER" prepare "$archive"
mkdir -p "$smoke_dir/shim"
cat > "$smoke_dir/shim/curl" <<'SHIM'
#!/usr/bin/env bash
exec /usr/bin/python3 "$SHIGUANG_SMOKE_HELPER" curl "$@"
SHIM
chmod 0755 "$smoke_dir/shim/curl"

# The only shim is curl: exact GitHub download URLs use the just-built archive;
# actual localhost health checks still use /usr/bin/curl.
PATH="$smoke_dir/shim:$PATH" bash "$script_dir/install.sh" --dir "$prefix"
[[ "$(systemctl show shiguang -p User --value)" == shiguang ]]
[[ "$(systemctl show shiguang -p Restart --value)" == always ]]
process_id="$(systemctl show shiguang -p MainPID --value)"
[[ "$process_id" =~ ^[1-9][0-9]*$ ]]
[[ "$(ps -o user= -p "$process_id" | tr -d ' ')" == shiguang ]]
python3 "$SHIGUANG_SMOKE_HELPER" initialize
printf '\n# ci-preserve-existing-config\n' >> "$prefix/.env"
python3 "$SHIGUANG_SMOKE_HELPER" capture

# Wrong checksum must fail before stopping the running service or changing data.
SHIGUANG_SMOKE_BAD_CHECKSUM=1 PATH="$smoke_dir/shim:$PATH" bash "$script_dir/install.sh" --dir "$prefix" --version v999.0.1 > "$smoke_dir/checksum-rejection.log" 2>&1 && { echo 'Corrupted checksum was accepted.' >&2; exit 1; }
systemctl is-active --quiet shiguang
python3 "$SHIGUANG_SMOKE_HELPER" unchanged

# An explicit --listen updates only that setting after backing up the original .env.
PATH="$smoke_dir/shim:$PATH" bash "$script_dir/install.sh" --dir "$prefix" --version v999.0.1 --listen 0.0.0.0:18081 --no-auto-update
SHIGUANG_SMOKE_BASE='http://127.0.0.1:18081' python3 "$SHIGUANG_SMOKE_HELPER" verify 0.0.0.0:18081
python3 "$SHIGUANG_SMOKE_HELPER" capture

# An upgrade without --listen preserves the custom port and update preferences.
PATH="$smoke_dir/shim:$PATH" bash "$script_dir/install.sh" --dir "$prefix" --version v999.0.1
SHIGUANG_SMOKE_BASE='http://127.0.0.1:18081' python3 "$SHIGUANG_SMOKE_HELPER" verify preserve 2
printf 'Linux systemd smoke passed: public default, non-root service, 25-minute default, administrator/CDK/key persistence, rejected checksum, upgrade, explicit listen change, backup.\n'
