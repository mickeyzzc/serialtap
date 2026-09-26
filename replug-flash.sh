#!/bin/bash
# replug-flash —— 监听 daemon 事件流（零端口操作），检测到 s3zero 重新接入
# （collector started）后立刻代理刷 v2（新启动黄金窗口：RX 缓冲为空）。
cd /c/Users/micke/Projects/embedded/serialtap
EV=logs/s3zero/events-20260922.log
LOG=/tmp/catch-flash.log
START_LINE=$(wc -l < "$EV" 2>/dev/null || echo 0)
: > $LOG
echo "$(date +%T) replug-flash armed（等待重新接入，无需按键）" >> $LOG
for i in $(seq 1 7200); do
  NEW=$(tail -n +"$((START_LINE + 1))" "$EV" 2>/dev/null | grep -c "collector s3zero] collector started")
  if [ "$NEW" -gt 0 ]; then
    echo "$(date +%T) 重新接入检测到 —— 等 2s 应用起跑后刷" >> $LOG
    sleep 2
    if ./serialtap.exe flash 4ea5ac3 \
      ../esp32-s3-zero/env-station/build/env-station.bin@0x10000 >> $LOG 2>&1; then
      echo "$(date +%T) FLASH OK" >> $LOG
      exit 0
    fi
    echo "$(date +%T) 首刷失败，5s 后再试一轮" >> $LOG
    sleep 5
    if ./serialtap.exe flash 4ea5ac3 \
      ../esp32-s3-zero/env-station/build/env-station.bin@0x10000 >> $LOG 2>&1; then
      echo "$(date +%T) FLASH OK (round 2)" >> $LOG
      exit 0
    fi
    echo "$(date +%T) 两轮均败（若长按过 BOOTLOADER 请手动说一声）" >> $LOG
    exit 1
  fi
  sleep 3
done
echo "$(date +%T) 6 小时无重插" >> $LOG
exit 1
