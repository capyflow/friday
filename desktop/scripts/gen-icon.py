#!/usr/bin/env python3
"""Friday 图标绘制器（docs/DESKTOP.md 第四节）。

纯 stdlib 画 PNG（无需设计资源、无需网络）：1024 底图绘制后盒式降采样，
产出 app-icon.png（1024 底图）与常用 PNG 尺寸；icon.icns / icon.ico 等规范
容器由 build-release.sh 调 `npx @tauri-apps/cli icon` 从底图生成（官方工具
保证 macOS/Windows 图标格式完全兼容）。

用法：gen-icon.py --out <目录>

品牌标基准：白底圆角方块 + 中心黑点，圆角 = 边长 28%、圆点 = 边长 44%。同一套几何还出现在
外壳启动页 ui/index.html 的 #logo、网页端 style.css 的 .brand-mark、以及 index.html 的 favicon；
改一处就要同步那三处（icons/ 目录本身是构建产物，被 gitignore）。
"""

import argparse
import math
import struct
import zlib
from pathlib import Path

BASE = 1024
MARGIN = 92                      # 内容占画布 82%（macOS 图标惯例：四周留透明边）
PLATE = BASE - 2 * MARGIN        # 840
PLATE_RADIUS = round(PLATE * 0.22)
DOT_RATIO = 0.44                 # 圆点直径 = 底板边长 44%（与外壳启动页 #logo 同一比例）
PLATE_CORNER_RATIO = 0.28        # 圆角 = 底板边长 28%（同上）
PLATE_RADIUS = round(PLATE * PLATE_CORNER_RATIO)
PLATE_FILL = (255, 255, 255)     # 白底
PLATE_EDGE = (222, 222, 228)     # 外圈 3px 发丝边：纯白块在浅色桌面上会糊掉
DOT_FILL = (24, 24, 27)          # #18181B 中心黑点
DOT_R = PLATE * DOT_RATIO / 2


def sd_rounded_box(px: float, py: float, half: float, rad: float) -> float:
    """居中圆角方块的带符号距离（负=在内）。"""
    qx = abs(px) - (half - rad)
    qy = abs(py) - (half - rad)
    return math.hypot(max(qx, 0.0), max(qy, 0.0)) + min(max(qx, qy), 0.0) - rad


def coverage(dist: float) -> float:
    """把 1px 边缘按距离折成覆盖率，1024 原尺寸下也不出现锯齿硬边。"""
    return max(0.0, min(1.0, 0.5 - dist))


def base_pixel(x: int, y: int) -> tuple[int, int, int, int]:
    """1024 底图：白底圆角方块 + 中心黑点，四角透明。"""
    px = x + 0.5 - BASE / 2
    py = y + 0.5 - BASE / 2
    half = PLATE / 2
    dp = sd_rounded_box(px, py, half, PLATE_RADIUS)
    alpha = coverage(dp)
    if alpha <= 0:
        return (0, 0, 0, 0)
    # 底板色：最外缘 3px 混入发丝边（纯白块在浅色桌面上会糊掉），往里立刻回到白
    d_in = -dp
    t = max(0.0, min(1.0, 1.0 - d_in / 3.0))
    rgb = [round(p + (e - p) * t) for p, e in zip(PLATE_FILL, PLATE_EDGE)]
    # 中心黑点
    ac = coverage(math.hypot(px, py) - DOT_R)
    rgb = [round(c + (d - c) * ac) for c, d in zip(rgb, DOT_FILL)]
    return (*rgb, round(alpha * 255))


def render_base() -> bytes:
    buf = bytearray()
    for y in range(BASE):
        for x in range(BASE):
            buf.extend(base_pixel(x, y))
    return bytes(buf)


def downsample(base: bytes, size: int) -> bytes:
    """盒式降采样（1024 → size），天然抗锯齿。"""
    f = BASE // size
    out = bytearray()
    for y in range(size):
        for x in range(size):
            rs = gs = bs = als = 0
            for dy in range(f):
                row = ((y * f + dy) * BASE + x * f) * 4
                for dx in range(f):
                    i = row + dx * 4
                    rs += base[i]
                    gs += base[i + 1]
                    bs += base[i + 2]
                    als += base[i + 3]
            n = f * f
            out.extend((rs // n, gs // n, bs // n, als // n))
    return bytes(out)


def write_png(path: Path, size: int, rgba: bytes) -> None:
    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data)) + tag + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    ihdr = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)  # 8bit RGBA
    raw = bytearray()
    stride = size * 4
    for y in range(size):
        raw.append(0)  # filter: none
        raw.extend(rgba[y * stride:(y + 1) * stride])
    path.write_bytes(
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
        + chunk(b"IEND", b"")
    )


def main() -> None:
    ap = argparse.ArgumentParser(description="绘制 Friday 图标底图与常用 PNG 尺寸")
    ap.add_argument("--out", default=str(Path(__file__).resolve().parent.parent / "src-tauri" / "icons"))
    args = ap.parse_args()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    base = render_base()
    sizes = {
        32: out / "32x32.png",
        128: out / "128x128.png",
        256: out / "128x128@2x.png",
    }
    pngs: dict[int, bytes] = {}
    for size, path in sizes.items():
        data = downsample(base, size)
        pngs[size] = data
        write_png(path, size, data)
        print(f"  {path.name}  {size}x{size}")

    # 1024 底图：交给 `npx @tauri-apps/cli icon` 生成规范的全套（含 icon.icns/icon.ico）。
    # 注意：不要手写 icns——手写结构 sips 能读但 iconutil/系统图标服务不认，
    # 会表现为安装后的 app "没有图标"。
    write_png(out / "app-icon.png", BASE, base)
    print(f"  app-icon.png 已写入 {out}（由 build-release.sh 调 tauri icon 出全套）")


if __name__ == "__main__":
    main()
