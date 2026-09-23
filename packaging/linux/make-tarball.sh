#!/usr/bin/env bash
# serialtap Linux tar.gz 打包：<版本> <serialtap 路径> <仓库根> <输出目录> <arch>
# Linux 不带托盘（systray 需 libappindicator）；常驻入口 = serialtap run +
# Web 面板（操作全覆盖）。包内附 systemd 用户服务示例。
set -euo pipefail

VER="${1:?版本}"
BIN="$(cd "$(dirname "${2:?二进制路径}")" && pwd)/$(basename "$2")"
ROOT="$(cd "${3:?仓库根}" && pwd)"
OUT="$(cd "${4:?输出目录}" && pwd)"
ARCH="${5:-amd64}"
NAME="serialtap-${VER}-linux-${ARCH}"
STAGE="$OUT/$NAME"

rm -rf "$STAGE" "$OUT/$NAME.tar.gz"
mkdir -p "$STAGE"
cp "$BIN" "$STAGE/serialtap"
cp "$ROOT/README.md" "$STAGE/" 2>/dev/null || true

cat > "$STAGE/serialtap.service" <<'UNIT'
# systemd 用户服务示例：~/.config/systemd/user/serialtap.service
#   systemctl --user enable --now serialtap
[Unit]
Description=serialtap USB serial daemon
After=graphical-session.target

[Service]
ExecStart=%h/.local/bin/serialtap run
Restart=on-failure

[Install]
WantedBy=default.target
UNIT

cat > "$STAGE/INSTALL.md" <<'DOC'
# 安装
install -m 755 serialtap ~/.local/bin/
# 常驻（推荐 systemd 用户服务）：
mkdir -p ~/.config/systemd/user && cp serialtap.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now serialtap
# 面板（全部操作 Web 化）：http://127.0.0.1:8801/
DOC

tar -C "$OUT" -czf "$OUT/$NAME.tar.gz" "$NAME"
rm -rf "$STAGE"
echo "OK: $OUT/$NAME.tar.gz"
