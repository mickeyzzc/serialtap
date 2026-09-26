#!/bin/bash
# catch-flash v7 —— 免 BOOT 恢复/刷机：停靠 + 验证 + 确认后免时序直刷。
# 段一：park.py 紧密循环抢开端口 → USB-Serial-JTAG 复位时序停进 ROM 下载模式。
# 段二：esptool --before no-reset chip-id 验证真在下载模式（park 的 trance
#       不保证成立）；验证过 → 直接 write-flash --before no-reset（跳过时序
#       握手，绕开 DTR/RTS 抖动和 CDC 写超时）；未验证 → 回退 serialtap 代理
#       刷（带 3×5s 重试）。
cd /c/Users/micke/Projects/embedded/serialtap
B=../esp32-s3-zero/env-station/build
PY=/c/Espressif/tools/python/v6.0/venv/Scripts/python.exe
ET=/c/Espressif/tools/python/v6.0/venv/Scripts/esptool.exe
LOG=/tmp/catch-flash.log

: > $LOG
echo "$(date +%T) v7 armed: park + verify + no-reset flash" >> $LOG
if ! "$PY" park.py COM6 --timeout "${1:-1200}" >> $LOG 2>&1; then
  echo "$(date +%T) park failed/timeout" >> $LOG
  exit 1
fi

if "$ET" --port COM6 --before no-reset --after no-reset chip-id 2>&1 | grep -q "Chip is"; then
  echo "$(date +%T) verified in download mode — no-reset direct flash" >> $LOG
  "$ET" --port COM6 --before no-reset --after hard-reset \
    write-flash 0x10000 "$B/env-station.bin" >> $LOG 2>&1
  rc=$?
  echo "$(date +%T) no-reset flash rc=$rc" >> $LOG
  exit $rc
fi

echo "$(date +%T) park NOT verified — fallback proxy flash" >> $LOG
./serialtap.exe flash 4ea5ac3 "$B/env-station.bin@0x10000" >> $LOG 2>&1
rc=$?
echo "$(date +%T) fallback flash rc=$rc" >> $LOG
exit $rc
