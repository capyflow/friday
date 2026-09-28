#!/usr/bin/env python3
"""Friday 图标绘制器（docs/DESKTOP.md 第四节）。

纯 stdlib 画 PNG（无需设计资源、无需网络）：1024 底图绘制后盒式降采样，
产出 app-icon.png（1024 底图）与常用 PNG 尺寸；icon.icns / icon.ico 等规范
容器由 build-release.sh 调 `npx @tauri-apps/cli icon` 从底图生成（官方工具
保证 macOS/Windows 图标格式完全兼容）。

用法：gen-icon.py --out <目录>
"""

import argparse
import math
import struct
import zlib
from pathlib import Path

BASE = 1024
# 品牌色：靛蓝 → 紫 → 紫红 对角渐变（与弹窗/占位页一致）
GRADIENT_STOPS = [
    (0.00, (99, 102, 241)),   # #6366F1
    (0.55, (139, 92, 246)),   # #8B5CF6
    (1.00, (196, 132, 252)),  # #C484FC
]
INK = (67, 56, 202)       # #4338CA 五官色
WHITE = (255, 255, 255)


def in_rounded_rect(x: float, y: float, l: float, t: float, r: float, b: float, rad: float) -> bool:
    cx = min(max(x, l + rad), r - rad)
    cy = min(max(y, t + rad), b - rad)
    return (x - cx) ** 2 + (y - cy) ** 2 <= rad * rad


def gradient(t: float) -> tuple[int, int, int]:
    for (t0, c0), (t1, c1) in zip(GRADIENT_STOPS, GRADIENT_STOPS[1:]):
        if t <= t1:
            k = (t - t0) / (t1 - t0)
            return tuple(round(a + (b - a) * k) for a, b in zip(c0, c1))
    return GRADIENT_STOPS[-1][1]


def base_pixel(x: int, y: int) -> tuple[int, int, int, int]:
    """1024 底图：渐变 squircle + 顶部光泽 + 柔和投影 + 白色机器人脸。"""
    m, rad = 64, 224
    if not in_rounded_rect(x, y, m, m, BASE - m, BASE - m, rad):
        return (0, 0, 0, 0)
    c = gradient((x + y) / (2 * BASE))

    # 顶部光泽：自上而下淡出的高光，让底色有玻璃感
    sheen = max(0.0, 1.0 - y / 460) * 0.10
    c = tuple(round(v + (255 - v) * sheen) for v in c)

    # 脸下方的柔和投影（高斯衰减椭圆），把主体从背景上"托"起来
    sd = ((x - 512) / 252) ** 2 + ((y - 776) / 54) ** 2
    if sd < 4.0:
        a = 78 * math.exp(-1.5 * sd) / 255
        c = tuple(round(v * (1 - a) + 40 * a) for v in c)

    # 五官先于头部判断（深色画在白色脸底之上）
    # 眼睛（竖椭圆，友好感）
    for cx in (420, 604):
        if ((x - cx) / 38) ** 2 + ((y - 500) / 56) ** 2 <= 1.0:
            return (*INK, 255)
    # 微笑
    if in_rounded_rect(x, y, 482, 578, 542, 598, 10):
        return (*INK, 255)
    # 天线球（深色点缀）
    if (x - 512) ** 2 + (y - 236) ** 2 <= 34 ** 2:
        return (*INK, 255)
    # 天线杆（连接球与头顶）
    if in_rounded_rect(x, y, 492, 256, 532, 360, 16):
        return (*WHITE, 255)
    # 两侧耳罩（探出头侧，保持轮廓可见）
    if in_rounded_rect(x, y, 248, 440, 304, 560, 26) or in_rounded_rect(x, y, 720, 440, 776, 560, 26):
        return (*WHITE, 255)
    # 头部
    if in_rounded_rect(x, y, 296, 336, 728, 664, 108):
        return (*WHITE, 255)
    return (*c, 255)


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
