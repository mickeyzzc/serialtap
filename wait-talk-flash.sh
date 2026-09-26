#!/bin/bash
# wait-talk-flash v1 —— 等 s3zero 串口恢复说话（拔插断电解锁后），自动代理刷 v2.5。
# 背景：2026-09-24 板子 USB 管道设备侧楔死（serial 全天 0 字节，ROM 不答 SYNC），
# 主机侧手段（reopen/pnputil/park/esptool usb-reset）全无效，只能拔插断电。
# 拔插后芯片重启进现刷的 v2.4 固件（常驻读串口）→ serialtap flash 一发即中。
cd /c/Users/micke/Projects/embedded/serialtap || exit 1
B=../esp32-s3-zero/env-station/build
DEADLINE=$(( $(date +%s) + 10800 ))   # 守 3 小时

while [ "$(date +%s)" -lt "$DEADLINE" ]; do
  LOG=$(ls -t logs/esp32s3-jtag/serial-*.log 2>/dev/null | head -1)
  n=$(wc -c < "$LOG" 2>/dev/null || echo 0)
  if [ "$n" -gt 200 ]; then
    echo "$(date +%T) 板子恢复说话（${n} 字节）—— 3s 后刷 v2.5（pt@0x8000 + app@0x20000）"
    sleep 3
    ./serialtap.exe flash 4ea5ac3 \
      "C:/Users/micke/Projects/embedded/esp32-s3-zero/env-station/build/partition_table/partition-table.bin@0x8000" \
      "C:/Users/micke/Projects/embedded/esp32-s3-zero/env-station/build/env-station.bin@0x20000"
    rc=$?
    echo "$(date +%T) flash rc=$rc —— 15s 后取串口尾部"
    sleep 15
    tail -20 "$LOG"
    exit $rc
  fi
  sleep 5
done
echo "timeout：3 小时内板子仍未说话"
exit 1
