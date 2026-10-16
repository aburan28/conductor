#!/usr/bin/env python3
"""Draw the Conductor.app icon and build AppIcon.icns from it.

    python3 macos/packaging/icon/make-icon.py

The mark matches the web dashboard favicon: a green rounded square (#2f6f4e) with
three white horizontal strokes (the queue / live-work mark). It sits on Apple's
macOS icon grid (824pt body on a 1024pt canvas with a soft drop shadow) so it
matches Dock neighbours.

The SVG is the source of truth for the mark; Pillow draws the bitmap so a Mac
build needs no Chromium. The .icns is committed so `macos/build.sh` needs nothing
beyond the Command Line Tools. Regenerating needs Pillow and icnsutil:

    python3 -m pip install pillow icnsutil
"""
from __future__ import annotations

import io
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# Brand green from internal/web/static/app.css --accent and the HTML favicon.
ACCENT = (0x2F, 0x6F, 0x4E, 0xFF)
ACCENT_TOP = (0x3A, 0x86, 0x5E, 0xFF)
STROKE = (0xFF, 0xFF, 0xFF, 0xFF)
CANVAS = 1024
BODY = 824
ORIGIN = (CANVAS - BODY) // 2  # 100
CORNER = 185  # Apple squircle-ish radius at this body size


def favicon_svg() -> str:
    """Compact SVG matching internal/web/static/index.html favicon (32×32 viewBox)."""
    return (
        "<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'>"
        "<rect width='32' height='32' rx='7' fill='#2f6f4e'/>"
        "<path d='M9 10h14M9 16h10M9 22h14' stroke='#fff' stroke-width='3' "
        "stroke-linecap='round'/></svg>\n"
    )


def app_icon_svg() -> str:
    """Full Dock icon: favicon mark on Apple's 1024 grid with a drop shadow."""
    x = y = ORIGIN
    # Line geometry scaled from the 32×32 favicon (padding 9/10, stroke 3).
    scale = BODY / 32
    stroke = 3 * scale
    lines = [
        (9 * scale, 10 * scale, 23 * scale, 10 * scale),
        (9 * scale, 16 * scale, 19 * scale, 16 * scale),
        (9 * scale, 22 * scale, 23 * scale, 22 * scale),
    ]
    path = " ".join(
        f"M{x + x0:.1f} {y + y0:.1f} L{x + x1:.1f} {y + y1:.1f}" for x0, y0, x1, y1 in lines
    )
    return f"""<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {CANVAS} {CANVAS}" width="{CANVAS}" height="{CANVAS}">
<defs>
  <linearGradient id="bg" x1="0" y1="0" x2="0" y2="1">
    <stop offset="0" stop-color="#3a865e"/>
    <stop offset="1" stop-color="#2f6f4e"/>
  </linearGradient>
  <filter id="shadow" x="-20%" y="-10%" width="140%" height="140%">
    <feGaussianBlur stdDeviation="14"/>
  </filter>
</defs>
<rect x="{x}" y="{y + 12}" width="{BODY}" height="{BODY}" rx="{CORNER}" fill="#000" opacity="0.28" filter="url(#shadow)"/>
<rect x="{x}" y="{y}" width="{BODY}" height="{BODY}" rx="{CORNER}" fill="url(#bg)"/>
<path d="{path}" fill="none" stroke="#ffffff" stroke-width="{stroke:.1f}" stroke-linecap="round"/>
<rect x="{x + 1}" y="{y + 1}" width="{BODY - 2}" height="{BODY - 2}" rx="{CORNER - 1}"
      fill="none" stroke="#ffffff" stroke-opacity="0.12" stroke-width="2"/>
</svg>
"""


def render_png() -> "Image.Image":
    from PIL import Image, ImageDraw, ImageFilter

    img = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
    x = y = ORIGIN

    # Drop shadow
    shadow = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
    sd = ImageDraw.Draw(shadow)
    sd.rounded_rectangle((x, y + 12, x + BODY, y + 12 + BODY), radius=CORNER, fill=(0, 0, 0, 90))
    shadow = shadow.filter(ImageFilter.GaussianBlur(14))
    img = Image.alpha_composite(img, shadow)

    # Gradient body: draw top colour then blend bottom
    body = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
    bd = ImageDraw.Draw(body)
    bd.rounded_rectangle((x, y, x + BODY, y + BODY), radius=CORNER, fill=ACCENT_TOP)
    bottom = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
    ImageDraw.Draw(bottom).rounded_rectangle(
        (x, y, x + BODY, y + BODY), radius=CORNER, fill=ACCENT
    )
    # Vertical fade from top colour to accent
    fade = Image.new("L", (CANVAS, CANVAS), 0)
    for row in range(y, y + BODY):
        t = (row - y) / max(BODY - 1, 1)
        ImageDraw.Draw(fade).line([(x, row), (x + BODY, row)], fill=int(255 * t))
    # Mask fade to the rounded rect
    mask = Image.new("L", (CANVAS, CANVAS), 0)
    ImageDraw.Draw(mask).rounded_rectangle((x, y, x + BODY, y + BODY), radius=CORNER, fill=255)
    fade = Image.composite(fade, Image.new("L", (CANVAS, CANVAS), 0), mask)
    body = Image.composite(bottom, body, fade)
    img = Image.alpha_composite(img, body)

    # Three strokes (favicon geometry × scale)
    draw = ImageDraw.Draw(img)
    scale = BODY / 32
    stroke = max(2, int(round(3 * scale)))
    segments = [
        (9, 10, 23, 10),
        (9, 16, 19, 16),
        (9, 22, 23, 22),
    ]
    for x0, y0, x1, y1 in segments:
        draw.line(
            (x + x0 * scale, y + y0 * scale, x + x1 * scale, y + y1 * scale),
            fill=STROKE,
            width=stroke,
        )
        # Caps so line ends look round at every size
        r = stroke / 2
        for cx, cy in ((x + x0 * scale, y + y0 * scale), (x + x1 * scale, y + y1 * scale)):
            draw.ellipse((cx - r, cy - r, cx + r, cy + r), fill=STROKE)

    # Subtle rim
    draw.rounded_rectangle(
        (x + 1, y + 1, x + BODY - 1, y + BODY - 1),
        radius=CORNER - 1,
        outline=(255, 255, 255, 30),
        width=2,
    )
    return img


def build_icns(full, out_path: str) -> None:
    from icnsutil import IcnsFile

    # Slightly bolder strokes for 16/32 so the mark stays readable in Finder list view.
    small = full.copy()
    icns = IcnsFile()
    for key, px in [
        ("ic04", 16),
        ("ic05", 32),
        ("ic11", 32),
        ("ic12", 64),
        ("ic07", 128),
        ("ic08", 256),
        ("ic13", 256),
        ("ic09", 512),
        ("ic14", 512),
        ("ic10", 1024),
    ]:
        from PIL import Image

        src = small if px <= 32 else full
        buf = io.BytesIO()
        src.resize((px, px), Image.LANCZOS).save(buf, "PNG", optimize=True)
        icns.add_media(key, data=buf.getvalue())
    icns.write(out_path)


def main() -> int:
    try:
        from PIL import Image  # noqa: F401
        from icnsutil import IcnsFile  # noqa: F401
    except ImportError:
        print("need pillow and icnsutil: python3 -m pip install pillow icnsutil", file=sys.stderr)
        return 1

    out = HERE
    open(os.path.join(out, "favicon.svg"), "w").write(favicon_svg())
    open(os.path.join(out, "AppIcon.svg"), "w").write(app_icon_svg())
    full = render_png()
    full.save(os.path.join(out, "AppIcon-1024.png"))
    build_icns(full, os.path.join(out, "AppIcon.icns"))
    # Menu-bar template (monochrome mark) for optional Swift use / docs.
    tray = full.resize((32, 32), Image.LANCZOS)
    tray.save(os.path.join(out, "MenuBarTemplate.png"))
    print(f"wrote AppIcon.icns, AppIcon-1024.png, AppIcon.svg, favicon.svg to {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
