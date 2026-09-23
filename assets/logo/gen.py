#!/usr/bin/env python3
"""serialtap logo 资产生成：app-icon.svg → PNG 多尺寸 + icon.ico（含托盘断连灰版）。

依赖：svglib + reportlab（纯 Python SVG 光栅化，免 cairo/Edge headless——
本机 Edge headless 连 --dump-dom 都不出活，不可用）、Pillow（ICO 组装）。
用法：python gen.py   （在本目录执行）
产物：
  app-icon-1024/256/64/48/32/24/16.png
  icon.ico        多尺寸（16/24/32/48/64/256）——托盘/快捷方式/安装包用
  icon-dim.ico    灰阶版（托盘"守护进程未运行"态）
  并把两个 ico 拷到 internal/tray/（go:embed 消费处）
"""

import os
import shutil

from PIL import Image, ImageEnhance
from svglib.svglib import svg2rlg
from reportlab.graphics import renderPM

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "app-icon.svg")
SIZES = [256, 64, 48, 32, 24, 16]


def render_png(svg_path: str, out_png: str, px: int) -> None:
    """svglib 光栅化 SVG → 指定尺寸 PNG（app-icon 自带深底，无透明需求）。"""
    drawing = svg2rlg(svg_path)
    scale = px / drawing.width
    drawing.scale(scale, scale)
    drawing.width = drawing.height = px
    renderPM.drawToFile(drawing, out_png, fmt="PNG")  # 72dpi：1pt=1px
    if not os.path.exists(out_png):
        raise RuntimeError(f"svglib 未产出 {out_png}")


def dim(img: Image.Image) -> Image.Image:
    """托盘断连态：去饱和 + 压暗（与旧版灰底图标的'离线灰'观感一致）。"""
    return ImageEnhance.Brightness(ImageEnhance.Color(img).enhance(0.0)).enhance(0.55)


def main() -> None:
    big = os.path.join(HERE, "app-icon-1024.png")
    render_png(SRC, big, 1024)
    src = Image.open(big)
    for s in SIZES:
        src.resize((s, s), Image.LANCZOS).save(os.path.join(HERE, f"app-icon-{s}.png"))

    icon = os.path.join(HERE, "icon.ico")
    src.resize((256, 256), Image.LANCZOS).save(
        icon, format="ICO",
        sizes=[(s, s) for s in (16, 24, 32, 48, 64, 256)])
    icon_dim = os.path.join(HERE, "icon-dim.ico")
    dim(src.resize((256, 256), Image.LANCZOS)).save(
        icon_dim, format="ICO",
        sizes=[(s, s) for s in (16, 24, 32, 48, 64, 256)])

    tray = os.path.normpath(os.path.join(HERE, "..", "..", "internal", "tray"))
    shutil.copy(icon, os.path.join(tray, "icon.ico"))
    shutil.copy(icon_dim, os.path.join(tray, "icon_dim.ico"))
    print("OK:", ", ".join(sorted(os.listdir(HERE))))


if __name__ == "__main__":
    main()
