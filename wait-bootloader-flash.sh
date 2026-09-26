#!/bin/bash
# 等用户长按右键进 BOOTLOADER，探测到即 no-reset 直刷 v2（无 trance，不会打僵）。
cd /c/Users/micke/Projects/embedded/serialtap
ET=/c/Espressif/tools/python/v6.0/venv/Scripts/esptool.exe
LOG=/tmp/catch-flash.log
: > $LOG
echo "$(date +%T) waiting for BOOTLOADER (right-button long-press)" >> $LOG
for i in $(seq 1 120); do
  if "$ET" --port COM6 --before no-reset --after no-reset chip-id 2>&1 | grep -q "Chip is"; then
    echo "$(date +%T) chip in bootloader — flashing v2" >> $LOG
    "$ET" --port COM6 --before no-reset --after hard-reset \
      write-flash 0x10000 ../esp32-s3-zero/env-station/build/env-station.bin >> $LOG 2>&1
    rc=$?
    echo "$(date +%T) flash rc=$rc" >> $LOG
    exit $rc
  fi
  sleep 2
done
echo "$(date +%T) gave up" >> $LOG
exit 1
