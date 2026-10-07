# 安装与更新

从 [GitHub Releases](https://github.com/MengStar-L/OpenAIMailTransaction/releases) 下载正式版本。页面、字体和图片均已内置，服务器无需安装 Go、Node.js 或数据库。

## Linux 安装

支持运行 systemd 的 Linux amd64 / arm64，默认安装在 `/opt/shiguang`，以专用普通用户 `shiguang` 运行。

```bash
curl -fL https://github.com/MengStar-L/OpenAIMailTransaction/releases/latest/download/install.sh -o install.sh
sudo bash install.sh
```

安装脚本下载对应架构的正式 Release，并校验 `checksums.txt` 中的 SHA-256 后安装。默认监听 `0.0.0.0:8080`，首次安装开启自动检查和自动更新。

可以固定版本、自定义 `/opt` 下的目录，或关闭首次安装的自动更新：

```bash
sudo bash install.sh --version v1.0.3 --dir /opt/shiguang --no-auto-update
```

安装后打开 `http://服务器IP:8080/admin` 设置管理员密码，再在后台设置中填写 SMSBower API 密钥、价格等参数。密码和 API 密钥不写进安装脚本或 `.env`。

已有安装若仍只监听本机，重新运行脚本并显式指定地址：

```bash
sudo bash install.sh --listen 0.0.0.0:8080
```

`--listen` 同样可用于指定其他地址或端口。使用 HTTPS 反向代理时可设置 `--listen 127.0.0.1:8080`，并在 `/opt/shiguang/.env` 将 `COOKIE_SECURE=true`、设置实际代理的 `TRUSTED_PROXIES` 后重启服务。反向代理应保留 `Host`，转发 `X-Forwarded-For` 与 `X-Forwarded-Proto`。安装脚本不修改防火墙、不申请证书。

再次执行安装脚本属于升级：保留管理员、API 密钥、CDK、订单及更新偏好。未指定 `--listen` 时保留已有监听地址；显式指定时只修改 `.env` 的 `LISTEN_ADDR`。`--no-auto-update` 仅影响首次安装。

## 日常管理

```bash
sudo systemctl status shiguang
sudo journalctl -u shiguang -f
sudo systemctl restart shiguang
sudo systemctl stop shiguang
```

| 位置 | 内容 |
| --- | --- |
| `/opt/shiguang/bin/shiguang` | 当前程序 |
| `/opt/shiguang/bin/shiguang.previous` | 上一次更新前的程序 |
| `/opt/shiguang/.env` | 监听地址、运行模式等配置 |
| `/opt/shiguang/data/live/` | 数据库、加密密钥、更新偏好与更新日志 |
| `/opt/shiguang/backups/` | 通过安装脚本升级前的完整备份，仅 root 可读 |
| `/etc/systemd/system/shiguang.service` | 服务配置 |

自定义安装目录时，将文档中的 `/opt/shiguang` 换成自己的目录。每台主机使用一个 `shiguang` 服务；变更安装目录应另行迁移数据和服务，不能把安装脚本当作目录迁移工具。

## 自动与手动更新

后台设置中的“程序更新”可手动检查 GitHub、安装新版本，并分别设置自动检查与自动更新。仅使用同仓库正式 Release，按操作系统和架构选择程序，下载后校验 SHA-256；遇到活动订单会延后更新，避免中断接码。更新后服务自动重启，重新登录后台即可查看新版本。关闭自动安装后仍可手动更新。

Linux 安装脚本部署的服务支持后台自动更新。二进制目录由服务用户写入，配置文件由 root 保管；systemd 负责进程退出后的重启。自定义服务需保持二进制目录可写并使用 `Restart=always`。容器部署应通过重新拉取或构建镜像更新。

也可以重新运行安装脚本升级到最新版本，或用 `--version` 指定版本：

```bash
sudo bash install.sh
sudo bash install.sh --version v1.0.3
```

手动脚本升级会先完成下载和校验，再停止服务、备份配置和整个数据目录、替换程序并检查健康状态。更新偏好保存在 `data/live/update-settings.json`，后续安装不会重置。

## 备份与回滚

完整备份应包含 `.env` 与整个 `data` 目录。数据库和 `secret.key` 必须一起保存，否则无法解密已保存的 API 密钥及 CDK。

```bash
sudo systemctl stop shiguang
sudo tar -czf /root/shiguang-backup.tar.gz -C /opt/shiguang .env data
sudo systemctl start shiguang
```

如需回退上一版程序，先在后台关闭自动更新，确认存在 `bin/shiguang.previous`：

```bash
sudo systemctl stop shiguang
sudo install -o shiguang -g shiguang -m 0755 /opt/shiguang/bin/shiguang.previous /opt/shiguang/bin/shiguang
sudo systemctl start shiguang
```

程序回滚保留当前数据；若某次新版包含不兼容的数据迁移，应停止服务后恢复对应版本的完整备份，不要混用不同备份中的数据库和密钥。恢复旧备份会回退备份之后的数据，应先另存当前数据。

## Windows / macOS

从 Release 下载对应的 ZIP / tar.gz 并核对 `checksums.txt`。将 `.env.example` 复制为 `.env`，设置 `APP_MODE=live`、`DATA_DIR=data/live`；在该目录运行 `shiguang.exe` 或 `./shiguang`，打开 `http://127.0.0.1:8080/admin`。已有安装只替换程序，保留 `.env` 与数据目录。后台会显示当前运行方式是否支持自动安装更新；Linux 一键安装是推荐的长期服务部署方式。

## 发布新版本

维护者推送 `v主版本.次版本.补丁版本` tag 后，GitHub Actions 自动测试、编译、打包，再发布 Release：

```bash
git tag -a v1.0.4 -m "v1.0.4"
git push origin v1.0.4
```

发布平台包括 Linux amd64 / arm64、Windows amd64、macOS amd64 / arm64。归档名称如 `shiguang_1.0.3_linux_amd64.tar.gz`，归档内包含程序、README、安装文档及配置示例；Release 同时提供 `install.sh` 和 `checksums.txt`。构建失败不会发布；上传完全部文件后才将草稿转为正式版本。已发布版本不覆盖，修正应使用新 tag。

在 Actions 手动运行该工作流仅生成构建产物，不创建正式 Release。
