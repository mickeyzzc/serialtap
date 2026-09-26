"""park.py v2 —— 把 ESP32 USB-Serial-JTAG 板停进 ROM 下载模式（免按 BOOT）。

v1 教训：pyserial 开端口时的 DTR/RTS 初始线态决定复位时序的"起跳角"。
esptool 以 (DTR=1,RTS=1) 打开端口，其 USBJTAGSerialReset 的每一步边沿都
以此为前提；v1 以 (0,0) 打开导致序列首拍无边沿、FSM 丢步，时序大多失效。

v2 流程（单次开端口内完成）：
  1. 以 rts=dtr=True 打开（与 esptool 同角）；
  2. 打 esptool 的 USBJTAGSerialReset 时序；
  3. 立即写 ROM SYNC 帧（07 07 12 20 + 64 字节 dummy），读回应答
     （C0 04 04 ...）即为真·下载模式；
  4. 未应答则回到 2 重试（最多 N 次），成功打印 PARKED 退出 0。
"""
import sys
import time

import serial

PORT = "COM6"
TIMEOUT_S = 600
ATTEMPTS = 12

SYNC = bytes([0x07, 0x07, 0x12, 0x20]) + b"\x55" * 64


def trance(s: serial.Serial) -> None:
    """esptool USBJTAGSerialReset 逐句复刻（前提：当前线态 (1,1)）。"""
    s.rts = False
    s.dtr = False
    time.sleep(0.1)
    s.dtr = True
    s.rts = False
    time.sleep(0.1)
    s.rts = True
    s.dtr = False
    s.rts = True
    time.sleep(0.1)
    s.dtr = False
    s.rts = False


def in_bootloader(s: serial.Serial) -> bool:
    s.reset_input_buffer()
    s.write(SYNC)
    try:
        r = s.read(8)
    except Exception:  # noqa: BLE001
        return False
    return len(r) >= 4 and r[0] == 0xC0 and r[1] == 0x04


def main() -> int:
    port, timeout = PORT, TIMEOUT_S
    args = sys.argv[1:]
    if args and not args[0].startswith("-"):
        port = args.pop(0)
    if "--timeout" in args:
        timeout = int(args[args.index("--timeout") + 1])

    print(f"park v2: {port}（同角起跳 + SYNC 验证）", flush=True)
    deadline = time.monotonic() + timeout
    opens = 0
    while time.monotonic() < deadline:
        s = serial.Serial()
        s.port = port
        s.baudrate = 115200
        s.timeout = 0.3
        s.rts = True
        s.dtr = True
        try:
            s.open()
        except Exception:  # noqa: BLE001 - 端口不在/被占 = 继续等
            time.sleep(0.05)
            continue
        opens += 1
        for i in range(ATTEMPTS):
            try:
                trance(s)
                if in_bootloader(s):
                    s.close()
                    print(f"PARKED（第 {opens} 次开端口 / 第 {i + 1} 拍时序）",
                          flush=True)
                    return 0
            except Exception as e:  # noqa: BLE001 - 端口中途掉线
                print(f"park: 链路异常 {e}，重开端口", flush=True)
                break
        try:
            s.close()
        except Exception:  # noqa: BLE001
            pass
        time.sleep(0.3)
    print("park: timeout", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
