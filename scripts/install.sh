#!/usr/bin/env bash
# Install an official, checksum-verified release. Never source downloaded/local configuration.
set -Eeuo pipefail
umask 077

readonly REPOSITORY='MengStar-L/OpenAIMailTransaction'
readonly SERVICE='shiguang'
readonly SERVICE_USER='shiguang'
install_dir='/opt/shiguang'
release_version='latest'
listen_addr='0.0.0.0:8080'
listen_explicit=false
auto_update=true
tmp_dir=''

fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'USAGE'
拾光 Linux 安装 / 升级
用法：sudo bash install.sh [选项]
  --version v1.0.2        安装指定稳定版本（默认 latest）
  --dir /opt/shiguang     安装目录，必须位于 /opt 下
  --listen HOST:PORT     监听地址（新安装默认 0.0.0.0:8080；升级时显式传入可修改）
  --no-auto-update        首次安装不启用自动更新，仍自动检查
  --help                 显示帮助
升级保留已有配置；只有显式 --listen 会修改 LISTEN_ADDR。
管理员、API 密钥、CDK 和自动更新偏好均保留。
USAGE
}

while (($#)); do
  case "$1" in
    --version|--dir|--listen)
      (($# >= 2)) || fail "$1 缺少参数"
      case "$1" in
        --version) release_version="$2" ;;
        --dir) install_dir="$2" ;;
        --listen) listen_addr="$2"; listen_explicit=true ;;
      esac
      shift 2
      ;;
    --no-auto-update) auto_update=false; shift ;;
    --help|-h) usage; exit 0 ;;
    *) fail "未知参数：$1" ;;
  esac
done

[[ "$release_version" == latest || "$release_version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail '版本必须为 latest 或 v主版本.次版本.补丁版本'
[[ "$install_dir" =~ ^/opt/[A-Za-z0-9_-][A-Za-z0-9_./-]*$ ]] || fail '安装目录必须是 /opt 下不含空格的独立目录'
[[ "$install_dir" != */../* && "$install_dir" != */.. && "$install_dir" != */./* && "$install_dir" != */. && "$install_dir" != */ ]] || fail '安装目录不能含有相对路径段或结尾斜线'
[[ "$listen_addr" =~ ^([A-Za-z0-9._-]+|\[[0-9a-fA-F:]+\])?:[0-9]{1,5}$ ]] || fail '监听地址格式应为 HOST:PORT'
listen_port="${listen_addr##*:}"
((10#$listen_port >= 1 && 10#$listen_port <= 65535)) || fail '端口范围为 1–65535'
[[ "$(uname -s)" == Linux ]] || fail '此脚本只适用于 Linux；Windows / macOS 请下载对应 Release'
[[ "$EUID" == 0 ]] || fail '请用 sudo bash install.sh 执行'
[[ -d /run/systemd/system ]] || fail '需要运行中的 systemd'
for command_name in curl tar sha256sum awk grep mktemp install id getent useradd systemctl realpath flock cp mv chmod chown; do
  command -v "$command_name" >/dev/null 2>&1 || fail "缺少命令：$command_name"
done
case "$(uname -m)" in
  x86_64|amd64) architecture=amd64 ;;
  aarch64|arm64) architecture=arm64 ;;
  *) fail '仅支持 Linux amd64 / arm64' ;;
esac
[[ "$(realpath -m -- "$install_dir")" == "$install_dir" ]] || fail '安装路径不能经过符号链接或非规范路径'
for controlled_path in "$install_dir/bin" "$install_dir/bin/shiguang" "$install_dir/bin/shiguang.previous" "$install_dir/bin/shiguang.new" "$install_dir/data" "$install_dir/data/live" "$install_dir/data/live/update-settings.json" "$install_dir/.env" "$install_dir/backups"; do
  [[ ! -L "$controlled_path" ]] || fail "安装路径不能是符号链接：$controlled_path"
done
unit_path="/etc/systemd/system/$SERVICE.service"
if [[ -e "$unit_path" ]] && ! grep -Fxq "ExecStart=$install_dir/bin/shiguang" "$unit_path"; then
  fail "已有 $SERVICE 服务使用其他路径；请使用其原 --dir，不覆盖其他服务"
fi
exec 9>"/run/lock/$SERVICE-install.lock"
flock -n 9 || fail '已有安装或升级正在进行'

cleanup() {
  if [[ -n "$tmp_dir" && "$tmp_dir" == /tmp/shiguang-install.* && -d "$tmp_dir" ]]; then
    rm -rf -- "$tmp_dir"
  fi
}
trap cleanup EXIT
tmp_dir="$(mktemp -d /tmp/shiguang-install.XXXXXXXX)"
download() {
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
    --connect-timeout 20 --max-time 300 --retry 3 "$1" --output "$2"
}
if [[ "$release_version" == latest ]]; then
  latest_url="$(curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
    --connect-timeout 20 --max-time 60 --output /dev/null --write-out '%{url_effective}' "https://github.com/$REPOSITORY/releases/latest")"
  [[ "$latest_url" == "https://github.com/$REPOSITORY/releases/tag/"* ]] || fail '无法确定最新正式版本'
  release_version="${latest_url##*/}"
fi
release_version="v${release_version#v}"
[[ "$release_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail '上游返回了不支持的版本号'
archive="shiguang_${release_version#v}_linux_${architecture}.tar.gz"
download_base="https://github.com/$REPOSITORY/releases/download/$release_version"
printf '下载 %s（%s）…\n' "$release_version" "$architecture"
download "$download_base/$archive" "$tmp_dir/$archive"
download "$download_base/checksums.txt" "$tmp_dir/checksums.txt"
expected_hash="$(awk -v name="$archive" '$2 == name || $2 == "*"name { print $1 }' "$tmp_dir/checksums.txt")"
[[ "$expected_hash" =~ ^[[:xdigit:]]{64}$ ]] || fail '校验文件缺失、重复或格式不正确'
printf '%s  %s\n' "$expected_hash" "$archive" > "$tmp_dir/selected-checksum.txt"
(cd "$tmp_dir" && sha256sum --check --status selected-checksum.txt) || fail '下载文件 SHA-256 校验失败'
[[ "$(tar -tzf "$tmp_dir/$archive" | grep -Fxc shiguang)" == 1 ]] || fail '发布包缺少唯一的 shiguang 程序'
[[ "$(tar -tvzf "$tmp_dir/$archive" -- shiguang)" == -* ]] || fail '发布包中的程序不是普通文件'
install -d -m 0700 "$tmp_dir/unpack"
tar -xzf "$tmp_dir/$archive" --no-same-owner --no-same-permissions -C "$tmp_dir/unpack" -- shiguang
[[ -f "$tmp_dir/unpack/shiguang" && ! -L "$tmp_dir/unpack/shiguang" && -s "$tmp_dir/unpack/shiguang" ]] || fail '发布包程序不合法'

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --user-group --home-dir "$install_dir" --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
[[ "$(id -u "$SERVICE_USER")" != 0 ]] || fail '服务账户不能使用 root 身份'
getent group "$SERVICE_USER" >/dev/null || fail "服务账户缺少同名组：$SERVICE_USER"
install -d -o root -g "$SERVICE_USER" -m 0750 "$install_dir"
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$install_dir/bin" "$install_dir/data" "$install_dir/data/live"
install -d -o root -g root -m 0700 "$install_dir/backups"

if [[ -e "$unit_path" ]]; then
  systemctl stop "$SERVICE"
fi
if [[ -f "$install_dir/bin/shiguang" || -f "$install_dir/.env" ]]; then
  backup_dir="$install_dir/backups/$(date -u +%Y%m%dT%H%M%SZ)-$$"
  install -d -o root -g root -m 0700 "$backup_dir"
  [[ ! -f "$install_dir/.env" ]] || cp -a -- "$install_dir/.env" "$backup_dir/.env"
  [[ ! -f "$install_dir/bin/shiguang" ]] || cp -a -- "$install_dir/bin/shiguang" "$backup_dir/shiguang"
  cp -a -- "$install_dir/data" "$backup_dir/data"
  printf '升级前备份：%s\n' "$backup_dir"
fi
if [[ ! -e "$install_dir/.env" ]]; then
  cat > "$install_dir/.env" <<ENV
APP_MODE=live
LISTEN_ADDR=$listen_addr
DATA_DIR=data/live
SMSBOWER_API_BASE=https://smsbower.page
COOKIE_SECURE=false
TRUSTED_PROXIES=
ENV
elif [[ "$listen_explicit" == true ]]; then
  # Change only the requested setting, after the original config has been backed up.
  # Read configuration as data; never source values or interpolate shell commands.
  awk -v address="$listen_addr" '
    /^[[:space:]]*LISTEN_ADDR[[:space:]]*=/ {
      if (!found++) print "LISTEN_ADDR=" address
      next
    }
    { print }
    END { if (!found) print "LISTEN_ADDR=" address }
  ' "$install_dir/.env" > "$tmp_dir/config.env"
  install -o root -g "$SERVICE_USER" -m 0640 "$tmp_dir/config.env" "$install_dir/.env"
fi
chown "root:$SERVICE_USER" "$install_dir/.env"
chmod 0640 "$install_dir/.env"
if [[ ! -e "$install_dir/data/live/update-settings.json" ]]; then
  printf '{"auto_check":true,"auto_update":%s}\n' "$auto_update" > "$install_dir/data/live/update-settings.json"
  chown "$SERVICE_USER:$SERVICE_USER" "$install_dir/data/live/update-settings.json"
  chmod 0600 "$install_dir/data/live/update-settings.json"
fi
if [[ -f "$install_dir/bin/shiguang" ]]; then
  install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$install_dir/bin/shiguang" "$install_dir/bin/shiguang.previous"
fi
install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$tmp_dir/unpack/shiguang" "$install_dir/bin/shiguang.new"
mv -f -- "$install_dir/bin/shiguang.new" "$install_dir/bin/shiguang"
cat > "$tmp_dir/$SERVICE.service" <<UNIT
[Unit]
Description=Shiguang temporary resource service
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
WorkingDirectory=$install_dir
ExecStart=$install_dir/bin/shiguang
Restart=always
RestartSec=3
TimeoutStopSec=90
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=$install_dir/bin $install_dir/data
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
UNIT
install -o root -g root -m 0644 "$tmp_dir/$SERVICE.service" "$unit_path"
systemctl daemon-reload
systemctl enable "$SERVICE"
systemctl start "$SERVICE"

# Read just the listen value, without executing .env contents.
configured_listen="$(awk '/^[[:space:]]*LISTEN_ADDR[[:space:]]*=/ {sub(/^[^=]*=/, ""); gsub(/^[[:space:]"\047]+|[[:space:]"\047]+$/, ""); print; exit}' "$install_dir/.env")"
if [[ "$configured_listen" =~ ^([A-Za-z0-9._-]+|\[[0-9a-fA-F:]+\])?:[0-9]{1,5}$ ]]; then
  health_host="${configured_listen%:*}"
  case "$health_host" in ''|0.0.0.0) health_host=127.0.0.1 ;; '[::]') health_host='[::1]' ;; esac
  health_url="http://$health_host:${configured_listen##*:}"
  healthy=false
  for ((attempt = 0; attempt < 30; attempt++)); do
    if curl --noproxy '*' --fail --silent --max-time 2 "$health_url/healthz" > /dev/null; then healthy=true; break; fi
    sleep 1
  done
  if [[ "$healthy" != true ]]; then
    fail "服务未通过健康检查；数据与升级前备份已保留。运行 journalctl -u $SERVICE -n 80 查看原因"
  fi
  printf '已安装 %s。管理后台：%s/admin\n' "$release_version" "$health_url"
  if [[ "${configured_listen%:*}" == 0.0.0.0 || "${configured_listen%:*}" == '[::]' || "${configured_listen%:*}" == '' ]]; then
    printf '已监听全部网卡；公网访问地址：http://服务器IP:%s/admin\n' "${configured_listen##*:}"
  fi
else
  systemctl is-active --quiet "$SERVICE" || fail '服务未启动，请查看 journalctl -u shiguang'
  printf '已安装 %s，请按保留的 .env 监听地址访问 /admin。\n' "$release_version"
fi
printf '首次访问 /admin 设置管理员密码，然后在设置中填写 API 密钥。\n'
printf '查看日志：sudo journalctl -u %s -f\n' "$SERVICE"
