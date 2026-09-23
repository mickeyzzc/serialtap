# serialtap Logo

**设计：Wave·Tap（方波 · 在线分接）**——UART 方波横贯（串口电平的通用
符号），低电平段中点垂直向下的抽头正是 serialtap 做的事：总线照常流动，
一旁分接观测。深底圆角方为应用图标惯例，深底保证浅色任务栏/浅色页面上
同样可见。

- 底色 `#0F172A`（深藏青）；方波 `#E2E8F0`（浅灰白）；抽头与端口点
  `#2DD4BF`（teal，示波器荧光，与面板主题色 `--teal:#39d2c0` 同族）
- 单色版（`variants/*.svg`）用 `currentColor`，任意底色可用

## 文件

| 文件 | 用途 |
|---|---|
| `app-icon.svg` | 应用图标源（深底圆角方 + Wave·Tap） |
| `icon.ico` | 多尺寸（16/24/32/48/64/256）—— **托盘**（internal/tray go:embed）、快捷方式、**安装包** |
| `icon-dim.ico` | 灰阶版——托盘"守护进程未运行"态 |
| `app-icon-{16..1024}.png` | 各尺寸位图（README/网页/商店图） |
| `variants/v1..v6.svg` | 6 个设计方向的变体（展示存档） |
| `showcase.html` | 设计探索展示页（本目录直接打开，无外网依赖） |
| `gen.py` | 重新生成全部位图与 ICO |

## 重新生成

```bash
pip install svglib reportlab rlPyCairo pillow   # 纯 Python，免 cairo/Edge
cd assets/logo && python gen.py
# 产物含 internal/tray/icon.ico 与 icon_dim.ico（go:embed 消费处，自动拷贝）
```

改 `app-icon.svg` 后重跑 `gen.py` 并重启托盘即可生效。安装包（Inno Setup /
NSIS / WiX）直接引用 `icon.ico`；exe 资源图标若要替换，可用 rsrc/
goversioninfo 把 `icon.ico` 编译进 `.syso`（当前未接）。
