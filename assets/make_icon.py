#!/usr/bin/env python3
"""生成 Bot Bureau 的标记（SVG）与全套应用图标。用法：python3 assets/make_icon.py

标记只定义一次：所有笔画都是「带四角半径的圆角矩形」或「圆」，同一份参数既拼成 SVG 的圆弧路径，
也压平成多边形交给 PIL 画。这样矢量和位图不会走样，而且前后两个 B 之间那道留白可以精确求出——
把前一个 B 的每个部件各自外扩 GAP 再并起来，等于把并集整体外扩（膨胀对并集可分配）。

图标按 iOS 26 的液态玻璃分层来搭，而不是画成一张平图：底层是纯色（浅色白、深色黑），
标记单独一层，高光和投影是从标记的遮罩推出来的第三层——顶边提亮、底边压暗、下方投影。
Icon Composer 要的是「不带任何效果的扁平分层」，所以 assets/icon-layers/ 里另存一套干净的层，
将来要出 .icon 直接拖进去，系统自己加玻璃材质。

macOS 那份仍然保留 80.5% 的留白与超椭圆裁形：.icns 是老格式，系统不会替它裁形，
铺满画布的话在 Dock 里会比邻居大一圈。Windows / Linux 期望铺满，所以两套分别出。

Generate Bot Bureau's mark (SVG) and the full icon set. Usage: python3 assets/make_icon.py

The mark is defined once: every stroke is a rounded rectangle with per-corner radii, or a circle.
One set of parameters becomes SVG arc paths and, flattened, polygons for PIL — so vector and raster
cannot drift apart. It also makes the gap between the two Bs exact: inflating each primitive of the
front B by GAP and unioning equals dilating the union, because dilation distributes over union.

The icons are assembled in layers the way iOS 26's Liquid Glass expects, rather than painted flat:
a plain ground (white in light, black in dark), the mark on its own layer, and a third layer of
speculars derived from the mark's own mask — lit top edge, shaded bottom edge, shadow beneath.
Icon Composer wants flat layers with no baked effects, so a clean set is also written to
assets/icon-layers/; drop those in when you want a real .icon and the system supplies the material.

The macOS build keeps its 80.5% padding and superellipse mask: .icns is the legacy format and the
system will not shape it for you, so a full-bleed square lands in the Dock a size larger than its
neighbours. Windows and Linux want the opposite, so both forms are emitted.
"""

import math
import pathlib
import shutil
import subprocess
import sys

from PIL import Image, ImageChops, ImageDraw, ImageFilter

ROOT = pathlib.Path(__file__).resolve().parent.parent
ASSETS = ROOT / "assets"
LAYERS = ASSETS / "icon-layers"
BUILD = ROOT / "app" / "build"

S = 1024
SS = 3                      # 超采样倍数，边缘才不会有锯齿 / supersampling so the edges do not alias
C = S * SS
PAD = 0.805                 # 苹果图标网格：本体约占画布 80% / Apple's grid gives the body ~80%

# 深浅两套。底色是纯白与纯黑（各带一丝渐变，纯平色在大尺寸下会显得死），
# 标记在黑底上提亮到同色相的浅棕，否则 #8A4A2D 压在黑上几乎看不见。
# Two appearances. The grounds are white and black (each with a whisper of gradient; a flat fill
# reads dead at large sizes), and on black the mark lightens to the same hue — #8A4A2D against
# black is nearly invisible.
THEMES = {
    "light": {
        "bg": ((255, 255, 255), (240, 240, 244)),
        "ink": ((150, 82, 50), (128, 66, 39)),      # 标记自身的竖向渐变 / gradient within the mark
        "flat": (138, 74, 45),                      # 分层图里的纯色 / the flat colour for the layers
        "shadow": (58, 30, 16, 58),
        "rim": (255, 255, 255, 76),
        "hem": (0, 0, 0, 24),
    },
    "dark": {
        "bg": ((22, 22, 26), (0, 0, 0)),
        "ink": ((216, 138, 92), (186, 108, 66)),
        "flat": (203, 124, 78),
        "shadow": (0, 0, 0, 120),
        "rim": (255, 255, 255, 88),
        "hem": (0, 0, 0, 40),
    },
}

# ---- 字形 / letterform, in design units --------------------------------------------------
STEM_L, STEM_R = 215.0, 375.0          # 主干 / the stem
SERIF_L = 190.0                        # 衬线小脚往左探出 / the serif nubs reach further left
TOP, BOT = 245.0, 955.0
WAIST = 575.0                          # 上下两个字碗的交界 / where the two bowls meet
TOP_R, BOT_R = 740.0, 795.0            # 两个字碗的外缘 / the outer edge of each bowl
NUB_H = 110.0

MARK_BOX = (190.0, 245.0, 1080.0, 955.0)    # 整个标记的外框 / the whole mark's bounding box
BACK_SCALE = 0.82                           # 后面那个 B 小一号 / the B behind is a size smaller
BACK_RIGHT, BACK_TOP = 1080.0, 370.0
GAP = 22.0                                  # 前后两个 B 之间的留白 / the gap between the two Bs

MARK_W = MARK_BOX[2] - MARK_BOX[0]
MARK_H = MARK_BOX[3] - MARK_BOX[1]


def rr(x0, y0, x1, y1, radii):
    return {"kind": "rr", "box": (x0, y0, x1, y1), "radii": tuple(float(r) for r in radii)}


def ci(cx, cy, r):
    return {"kind": "ci", "c": (cx, cy), "r": float(r)}


def b_outer():
    """字母 B 的实心轮廓 / the solid B silhouette."""
    return [
        rr(STEM_L, TOP, STEM_R, BOT, (0, 0, 0, 0)),
        rr(SERIF_L, TOP, STEM_R, TOP + NUB_H, (18, 0, 0, 0)),
        rr(SERIF_L, BOT - NUB_H, STEM_R, BOT, (0, 0, 0, 18)),
        rr(STEM_L, TOP, TOP_R, WAIST, (0, (WAIST - TOP) / 2, (WAIST - TOP) / 2, 0)),
        rr(STEM_L, WAIST, BOT_R, BOT, (0, (BOT - WAIST) / 2, (BOT - WAIST) / 2, 0)),
    ]


def b_counters():
    """字碗里的挖空：上面一个 D 形，下面一块面板 / the counters: a D above, a panel below."""
    return [
        rr(STEM_R, 345, 620, 540, (28, 97, 97, 28)),
        rr(STEM_R, 650, 700, 870, (110, 110, 110, 110)),
    ]


def b_eyes():
    return [ci(479, 760, 50), ci(595, 760, 50)]


def xform(prims, scale=1.0, dx=0.0, dy=0.0, inflate=0.0):
    out = []
    for p in prims:
        if p["kind"] == "rr":
            x0, y0, x1, y1 = p["box"]
            out.append(rr(dx + x0 * scale - inflate, dy + y0 * scale - inflate,
                          dx + x1 * scale + inflate, dy + y1 * scale + inflate,
                          [r * scale + inflate for r in p["radii"]]))
        else:
            cx, cy = p["c"]
            out.append(ci(dx + cx * scale, dy + cy * scale, p["r"] * scale + inflate))
    return out


def back(prims, inflate=0.0):
    dx = BACK_RIGHT - (BOT_R - SERIF_L) * BACK_SCALE - SERIF_L * BACK_SCALE
    dy = BACK_TOP - TOP * BACK_SCALE
    return xform(prims, BACK_SCALE, dx, dy, inflate)


def mark_layers():
    """标记的画法，按顺序叠：加、减、加。后面那个 B 只是个影子，不给脸。
    The mark, drawn in order — add, subtract, add. The B behind is faceless; the face belongs to
    whichever one is standing in front."""
    return [
        (back(b_outer()), True),
        (back(b_counters()), False),
        (xform(b_outer(), inflate=GAP), False),
        (b_outer(), True),
        (b_counters(), False),
        (b_eyes(), True),
    ]


# ---- 位图 / raster -----------------------------------------------------------------------
def rr_points(box, radii, steps=44):
    x0, y0, x1, y1 = box
    lim = min(x1 - x0, y1 - y0) / 2
    tl, tr, br, bl = [max(0.0, min(r, lim)) for r in radii]
    pts = []

    def arc(cx, cy, r, a0, a1):
        if r <= 0:
            pts.append((cx, cy))
            return
        for i in range(steps + 1):
            a = math.radians(a0 + (a1 - a0) * i / steps)
            pts.append((cx + r * math.cos(a), cy + r * math.sin(a)))

    arc(x0 + tl, y0 + tl, tl, 180, 270)
    arc(x1 - tr, y0 + tr, tr, 270, 360)
    arc(x1 - br, y1 - br, br, 0, 90)
    arc(x0 + bl, y1 - bl, bl, 90, 180)
    return pts


def paint(d, prims, k, ox, oy, fill):
    for p in prims:
        if p["kind"] == "rr":
            d.polygon([(ox + x * k, oy + y * k) for x, y in rr_points(p["box"], p["radii"])],
                      fill=fill)
        else:
            cx, cy = p["c"]
            r = p["r"]
            d.ellipse([ox + (cx - r) * k, oy + (cy - r) * k,
                       ox + (cx + r) * k, oy + (cy + r) * k], fill=fill)


def mark_mask(size, width_frac):
    """把标记按宽度比例放进画布中央，返回它的 alpha 遮罩。
    Scale the mark to a fraction of the canvas, centre it, and return its alpha mask."""
    k = size * width_frac / MARK_W
    ox = (size - MARK_W * k) / 2 - MARK_BOX[0] * k
    oy = (size - MARK_H * k) / 2 - MARK_BOX[1] * k
    m = Image.new("L", (size, size), 0)
    d = ImageDraw.Draw(m)
    for prims, add in mark_layers():
        paint(d, prims, k, ox, oy, 255 if add else 0)
    return m


def squircle_mask(size, n=5.0):
    """超椭圆遮罩 |x|^n + |y|^n = 1。n=5 接近 macOS 的转角曲率；n=2 是正圆，n→∞ 是方角。
    A superellipse mask |x|^n + |y|^n = 1. n=5 approximates macOS corner curvature; n=2 is a circle,
    n→∞ a square."""
    mask = Image.new("L", (size, size), 0)
    ImageDraw.Draw(mask).polygon(squircle_points(size, n), fill=255)
    return mask


def squircle_points(size, n=5.0, steps=2048):
    half = size / 2
    pts = []
    for i in range(steps + 1):
        theta = 2 * math.pi * i / steps
        ct, st = math.cos(theta), math.sin(theta)
        x = half * (abs(ct) ** (2 / n)) * (1 if ct >= 0 else -1)
        y = half * (abs(st) ** (2 / n)) * (1 if st >= 0 else -1)
        pts.append((half + x, half + y))
    return pts


def vgrad(size, c0, c1):
    """竖向渐变 / a vertical gradient."""
    ramp = Image.new("L", (1, size))
    ramp.putdata([int(255 * y / (size - 1)) for y in range(size)])
    return Image.composite(Image.new("RGBA", (size, size), c1 + (255,)),
                           Image.new("RGBA", (size, size), c0 + (255,)),
                           ramp.resize((size, size)))


def shifted(mask, dy):
    return mask.transform(mask.size, Image.AFFINE, (1, 0, 0, 0, 1, -dy))


def tinted(size, mask, rgba):
    layer = Image.new("RGBA", (size, size), rgba[:3] + (0,))
    layer.putalpha(mask.point(lambda v: v * rgba[3] // 255))
    return layer


def glass(size, mask, theme):
    """液态玻璃的三件套：下方投影、顶边提亮、底边压暗——全都从标记自己的遮罩推出来。
    Liquid Glass in three pieces — shadow beneath, lit top edge, shaded bottom edge — every one of
    them derived from the mark's own mask, so they follow the letterform exactly."""
    out = Image.new("RGBA", (size, size), (0, 0, 0, 0))

    drop = shifted(mask, size * 0.012).filter(ImageFilter.GaussianBlur(size * 0.024))
    out.alpha_composite(tinted(size, drop, theme["shadow"]))

    body = vgrad(size, *theme["ink"])
    body.putalpha(mask)
    out.alpha_composite(body)

    # 高光糊过之后必须再乘回标记本身，否则会漫到轮廓外面，看着就成了一圈外发光而不是玻璃的边。
    # The speculars are multiplied back by the mark after blurring; left alone they bleed past the
    # outline and read as an outer glow instead of a lit edge.
    rim = size * 0.009
    blur = ImageFilter.GaussianBlur(size * 0.005)
    top = ImageChops.multiply(ImageChops.subtract(mask, shifted(mask, rim)).filter(blur), mask)
    hem = ImageChops.multiply(ImageChops.subtract(mask, shifted(mask, -rim)).filter(blur), mask)
    out.alpha_composite(tinted(size, top, theme["rim"]))
    out.alpha_composite(tinted(size, hem, theme["hem"]))
    return out


def build(theme, size, width_frac):
    """一层底 + 一层标记 + 一层高光 / one ground layer, one mark layer, one specular layer."""
    img = vgrad(size, *theme["bg"])
    img.alpha_composite(glass(size, mark_mask(size, width_frac), theme))
    return img


def mac_icon(theme):
    """macOS 版：超椭圆裁形 + 四周留白 / macOS build: superellipse mask plus padding."""
    body_px = int(C * PAD)
    body = build(theme, body_px, 0.66)
    body.putalpha(squircle_mask(body_px))
    canvas = Image.new("RGBA", (C, C), (0, 0, 0, 0))
    off = (C - body_px) // 2
    canvas.paste(body, (off, off), body)
    return canvas.resize((S, S), Image.LANCZOS)


def square_icon(theme):
    """Windows / Linux 版：铺满画布，系统自己处理外形 / fills the canvas; those systems shape it."""
    return build(theme, C, 0.54).resize((S, S), Image.LANCZOS)


def flat_layers(theme):
    """给 Icon Composer 的扁平层：底色一张、标记一张，都不带效果、铺满画布。
    Flat layers for Icon Composer: one ground, one mark, no effects, full bleed."""
    ground = Image.new("RGBA", (S, S), theme["bg"][0] + (255,))
    mark = Image.new("RGBA", (S, S), theme["flat"] + (0,))
    mark.putalpha(mark_mask(C, 0.54).resize((S, S), Image.LANCZOS))
    return ground, mark


# ---- 矢量 / vector -----------------------------------------------------------------------
def svg_path(p, dx, dy):
    if p["kind"] == "ci":
        cx, cy = p["c"]
        r = p["r"]
        return (f'<circle cx="{cx + dx:.2f}" cy="{cy + dy:.2f}" r="{r:.2f}"')
    x0, y0, x1, y1 = p["box"]
    x0, y0, x1, y1 = x0 + dx, y0 + dy, x1 + dx, y1 + dy
    lim = min(x1 - x0, y1 - y0) / 2
    tl, tr, br, bl = [max(0.0, min(r, lim)) for r in p["radii"]]
    d = [f"M{x0 + tl:.2f},{y0:.2f}", f"H{x1 - tr:.2f}"]
    if tr:
        d.append(f"A{tr:.2f},{tr:.2f} 0 0 1 {x1:.2f},{y0 + tr:.2f}")
    d.append(f"V{y1 - br:.2f}")
    if br:
        d.append(f"A{br:.2f},{br:.2f} 0 0 1 {x1 - br:.2f},{y1:.2f}")
    d.append(f"H{x0 + bl:.2f}")
    if bl:
        d.append(f"A{bl:.2f},{bl:.2f} 0 0 1 {x0:.2f},{y1 - bl:.2f}")
    d.append(f"V{y0 + tl:.2f}")
    if tl:
        d.append(f"A{tl:.2f},{tl:.2f} 0 0 1 {x0 + tl:.2f},{y0:.2f}")
    return '<path d="' + " ".join(d) + 'Z"'


def svg_mask(dx, dy, indent="    "):
    """挖空不用 fill-rule：轮廓自己就互相重叠，偶奇填充会挖错地方，所以走遮罩，
    顺序和位图那边完全一致。
    The counters cannot use fill-rule: the silhouette pieces overlap each other, so even-odd would
    punch holes in the wrong places. A mask instead, in the same order the raster uses."""
    rows = [f'{indent}<rect x="0" y="0" width="{MARK_W:.0f}" height="{MARK_H:.0f}" fill="#000"/>']
    for prims, add in mark_layers():
        paint_color = "#fff" if add else "#000"
        for p in prims:
            rows.append(f'{indent}{svg_path(p, dx, dy)} fill="{paint_color}"/>')
    return "\n".join(rows)


def svg_mark(color, background=None, pad=0.0):
    """standalone 标记 / the mark on its own, optionally on a squircle ground."""
    w = MARK_W + pad * 2
    h = MARK_H + pad * 2
    dx, dy = -MARK_BOX[0] + pad, -MARK_BOX[1] + pad
    body = "" if background is None else (
        f'  <rect x="0" y="0" width="{w:.0f}" height="{h:.0f}" fill="{background}"/>\n')
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{w:.0f}" height="{h:.0f}" '
            f'viewBox="0 0 {w:.0f} {h:.0f}">\n'
            f'  <defs>\n    <mask id="bb" maskUnits="userSpaceOnUse" '
            f'x="0" y="0" width="{w:.0f}" height="{h:.0f}">\n'
            f'{svg_mask(dx, dy, indent="      ")}\n    </mask>\n  </defs>\n'
            f'{body}'
            f'  <rect x="0" y="0" width="{w:.0f}" height="{h:.0f}" fill="{color}" '
            f'mask="url(#bb)"/>\n</svg>\n')


def svg_icon(theme_name, theme):
    """整块图标的矢量版：超椭圆底 + 标记，比例和 .icns 那份一致。
    The whole tile as vector: superellipse ground plus mark, proportioned like the .icns build."""
    ink = "#%02x%02x%02x" % theme["flat"]
    ground = "#%02x%02x%02x" % theme["bg"][0]
    body = S * PAD
    frac = 0.66
    k = body * frac / MARK_W
    ox = (S - MARK_W * k) / 2 - MARK_BOX[0] * k
    oy = (S - MARK_H * k) / 2 - MARK_BOX[1] * k
    pts = " ".join(f"{x + (S - body) / 2:.1f},{y + (S - body) / 2:.1f}"
                   for x, y in squircle_points(int(body), steps=512))
    rows = [f'      <rect x="0" y="0" width="{S}" height="{S}" fill="#000"/>']
    for prims, add in mark_layers():
        color = "#fff" if add else "#000"
        for p in prims:
            q = xform([p], scale=k, dx=ox, dy=oy)[0]
            rows.append(f'      {svg_path(q, 0, 0)} fill="{color}"/>')
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{S}" height="{S}" '
            f'viewBox="0 0 {S} {S}">\n'
            f'  <defs>\n    <mask id="bb" maskUnits="userSpaceOnUse" '
            f'x="0" y="0" width="{S}" height="{S}">\n' + "\n".join(rows) +
            f'\n    </mask>\n  </defs>\n'
            f'  <polygon points="{pts}" fill="{ground}"/>\n'
            f'  <rect x="0" y="0" width="{S}" height="{S}" fill="{ink}" mask="url(#bb)"/>\n'
            f'</svg>\n')


def main() -> None:
    ASSETS.mkdir(exist_ok=True)
    LAYERS.mkdir(parents=True, exist_ok=True)
    BUILD.mkdir(parents=True, exist_ok=True)

    (ASSETS / "logo.svg").write_text(svg_mark("#8a4a2d"))
    (ASSETS / "logo-dark.svg").write_text(svg_mark("#cb7c4e"))

    for name, theme in THEMES.items():
        mac_icon(theme).save(ASSETS / f"icon-{name}.png")
        square_icon(theme).save(ASSETS / f"icon-square-{name}.png")
        (ASSETS / f"icon-{name}.svg").write_text(svg_icon(name, theme))
        ground, mark = flat_layers(theme)
        ground.save(LAYERS / f"background-{name}.png")
        mark.save(LAYERS / f"foreground-{name}.png")

    # 打包用的主图标走深色版：.icns 只能带一张图，而 Dock 与任务栏的底色偏深，深色版在两者上都稳。
    # 浅色版也放进 build/：应用跑起来之后，主进程按系统外观换 Dock 图标，两张都得随包分发。
    # The dark build is the primary icon for the package: an .icns carries a single image, and Dock
    # and taskbar materials skew dark, so the dark build holds up on both. The light build ships in
    # build/ as well — the main process swaps the Dock icon by system appearance while the app runs,
    # which needs both images inside the package.
    mac = mac_icon(THEMES["dark"])
    square = square_icon(THEMES["dark"])
    mac.save(ASSETS / "icon.png")
    mac.resize((512, 512), Image.LANCZOS).save(BUILD / "icon.png")
    mac_icon(THEMES["light"]).resize((512, 512), Image.LANCZOS).save(BUILD / "icon-light.png")
    square.save(BUILD / "icon.ico", sizes=[(s, s) for s in (16, 32, 48, 64, 128, 256)])

    if shutil.which("iconutil"):
        iconset = BUILD / "icon.iconset"
        shutil.rmtree(iconset, ignore_errors=True)
        iconset.mkdir()
        for base in (16, 32, 128, 256, 512):
            mac.resize((base, base), Image.LANCZOS).save(iconset / f"icon_{base}x{base}.png")
            mac.resize((base * 2, base * 2), Image.LANCZOS).save(iconset / f"icon_{base}x{base}@2x.png")
        subprocess.run(["iconutil", "-c", "icns", str(iconset), "-o", str(BUILD / "icon.icns")], check=True)
        shutil.rmtree(iconset)
    else:
        print("iconutil unavailable, skipping .icns", file=sys.stderr)

    print("mark written to", ASSETS / "logo.svg")
    print("flat layers for Icon Composer in", LAYERS)
    print("icons written to", BUILD)


if __name__ == "__main__":
    main()
