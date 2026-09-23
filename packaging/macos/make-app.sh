#!/usr/bin/env bash
# serialtap macOS .app + DMG 打包（在 macOS runner 上执行；本机为 CI 调用，
# 参数：<版本> <lipo 后的 serialtap 路径> <logo PNG 目录> <输出目录>）。
# .app 双击 = serialtap run（守护 + 菜单栏托盘；LSUIElement 隐藏 Dock 图标）。
set -euo pipefail

VER="${1:?版本}"
BIN="$(cd "$(dirname "${2:?二进制路径}")" && pwd)/$(basename "$2")"
LOGO="$(cd "${3:?logo PNG 目录}" && pwd)"
OUT="$(cd "${4:?输出目录}" && pwd)"
APP="$OUT/serialtap.app"

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

# 图标：logo PNG 各尺寸 → iconset → icns（iconutil 仅 macOS）
ICONSET="$OUT/serialtap.iconset"
rm -rf "$ICONSET"; mkdir -p "$ICONSET"
for spec in 16 32 128 256 512; do
  cp "$LOGO/app-icon-$spec.png" "$ICONSET/icon_${spec}x${spec}.png"
  spec2=$((spec * 2))
  [ -f "$LOGO/app-icon-$spec2.png" ] && cp "$LOGO/app-icon-$spec2.png" "$ICONSET/icon_${spec}x${spec}@2x.png" || true
done
cp "$LOGO/app-icon-1024.png" "$ICONSET/icon_512x512@2x.png"
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/serialtap.icns"
rm -rf "$ICONSET"

cp "$BIN" "$APP/Contents/MacOS/serialtap"
chmod +x "$APP/Contents/MacOS/serialtap"

# 双击入口：Finder 无法传参，用壳包装跑 `serialtap run` —— macOS 菜单栏托盘
# 内嵌在守护进程里（Host 回调注入，见 internal/tray/tray.go），run 即菜单栏。
cat > "$APP/Contents/MacOS/serialtap-tray" <<'WRAPPER'
#!/bin/sh
DIR="$(cd "$(dirname "$0")" && pwd)"
exec "$DIR/serialtap" run
WRAPPER
chmod +x "$APP/Contents/MacOS/serialtap-tray"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>serialtap</string>
  <key>CFBundleDisplayName</key><string>serialtap</string>
  <key>CFBundleIdentifier</key><string>com.mickeyzzc.serialtap</string>
  <key>CFBundleVersion</key><string>${VER}</string>
  <key>CFBundleShortVersionString</key><string>${VER}</string>
  <key>CFBundleExecutable</key><string>serialtap-tray</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleIconFile</key><string>serialtap.icns</string>
  <key>LSUIElement</key><true/>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

# DMG：拖拽安装的惯例载体
DMG="$OUT/serialtap-${VER}-darwin.dmg"
rm -f "$DMG"
hdiutil create -volname "serialtap" -srcfolder "$APP" -ov -format UDZO "$DMG"
echo "OK: $DMG"
