#!/usr/bin/env bash
# These cases fail before network requests, system changes, or root requirements.
set -euo pipefail
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
installer="$script_dir/install.sh"
help_text="$(bash "$installer" --help)"
[[ "$help_text" == *'0.0.0.0:8080'* ]]
[[ "$help_text" == *'显式 --listen'* ]]
expect_rejected() {
  if bash "$installer" "$@" >/dev/null 2>&1; then
    printf 'Expected rejection: %s\n' "$*" >&2
    exit 1
  fi
}
expect_rejected --unknown
expect_rejected --version
expect_rejected --listen
expect_rejected --version '../main'
expect_rejected --version 'v1.2.3;echo unsafe'
expect_rejected --dir /
expect_rejected --dir /opt
expect_rejected --dir /opt/../etc
expect_rejected --dir /opt/shiguang/../../etc
expect_rejected --dir '/opt/has space'
expect_rejected --listen '0.0.0.0:99999'
expect_rejected --listen '0.0.0.0:0'
expect_rejected --listen 'bad;command:8080'
printf 'Installer argument checks passed.\n'
