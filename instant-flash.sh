#!/bin/bash
# instant-flash —— 设备一出现在 list 立即代理刷（抢新启动黄金窗口），
# 外层最多 20 轮（覆盖拔插后前几分钟）。
cd /c/Users/micke/Projects/embedded/serialtap
LOG=/tmp/catch-flash.log
: > $LOG
echo "$(date +%T) instant-flash armed" >> $LOG
for i in $(seq 1 20); do
  if ./serialtap.exe list 2>/dev/null | grep -q 4ea5ac3; then
    echo "$(date +%T) round $i: device present — flash" >> $LOG
    if ./serialtap.exe flash 4ea5ac3 \
      ../esp32-s3-zero/env-station/build/env-station.bin@0x10000 >> $LOG 2>&1; then
      echo "$(date +%T) FLASH OK" >> $LOG
      exit 0
    fi
    echo "$(date +%T) round $i failed" >> $LOG
    sleep 2
  else
    sleep 0.5
  fi
done
echo "$(date +%T) instant-flash exhausted" >> $LOG
exit 1
