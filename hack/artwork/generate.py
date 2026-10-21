#!/usr/bin/env python3
"""Generate the KubeAmber logo kit as SVG.

    python generate.py SpaceGrotesk[wght].ttf ../../artwork

Needs fonttools and uharfbuzz. The wordmark is set in Space Grotesk Bold
(SIL Open Font License, https://github.com/floriankarsten/space-grotesk) and
converted to outlines, so the SVGs render the same everywhere without the
font. The icon geometry below is the source of truth; edit it here and
regenerate rather than editing the SVGs by hand.
"""

import os
import sys

import uharfbuzz as hb
from fontTools.pens.boundsPen import BoundsPen
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont

# --- Palette -----------------------------------------------------------------

INK = "#1C1917"  # charcoal: the shield on light grounds, "Kube" on light
PAPER = "#FAFAF9"  # "Kube" on dark grounds
AMBER = "#F59E0B"  # the lit lamp; "Amber" on dark grounds
AMBER_TEXT = "#B45309"  # "Amber" on light grounds (4.5:1 on white)
RED = "#EF4444"
GREEN = "#22C55E"
SHIELD_DARK = "#292524"  # the shield on dark grounds
SHIELD_DARK_EDGE = "#57534E"

# --- Icon (64 x 64 units) ----------------------------------------------------

SHIELD = "M32 3 L55 11 V29 C55 44 45 55 32 61 C19 55 9 44 9 29 V11 Z"
# Inset by half the 1.5 stroke, so the dark variant's edge stays inside 64 x 64.
SHIELD_INSET = "M32 3.8 L54.25 11.55 V29 C54.25 43.6 44.6 54.2 32 60.2 C19.4 54.2 9.75 43.6 9.75 29 V11.55 Z"
LAMPS = (18, 31, 44)  # top (stop), middle (hold, lit), bottom (go)
LAMP_R = 5.5
HALO_R = 8


def circle(cx, cy, r, fill, opacity=None):
    o = f' opacity="{opacity}"' if opacity is not None else ""
    return f'<circle cx="{cx}" cy="{cy}" r="{r}" fill="{fill}"{o}/>'


def circle_path(cx, cy, r):
    return f"M{cx - r} {cy} a{r} {r} 0 1 0 {2 * r} 0 a{r} {r} 0 1 0 {-2 * r} 0 Z"


def icon(variant):
    """The icon's SVG elements in 64 x 64 units."""
    top, mid, bottom = LAMPS
    if variant == "color":
        return "".join([
            f'<path d="{SHIELD}" fill="{INK}"/>',
            circle(32, top, LAMP_R, RED, 0.3),
            circle(32, mid, HALO_R, AMBER, 0.28),
            circle(32, mid, LAMP_R, AMBER),
            circle(32, bottom, LAMP_R, GREEN, 0.3),
        ])
    if variant == "color-dark":
        return "".join([
            f'<path d="{SHIELD_INSET}" fill="{SHIELD_DARK}" stroke="{SHIELD_DARK_EDGE}" stroke-width="1.5" stroke-linejoin="round"/>',
            circle(32, top, LAMP_R, RED, 0.35),
            circle(32, mid, HALO_R, AMBER, 0.3),
            circle(32, mid, LAMP_R, AMBER),
            circle(32, bottom, LAMP_R, GREEN, 0.35),
        ])
    # One colour: the shield with the stop and go lamps cut out, and the hold
    # lamp cut out as a ring around a solid dot, so it still reads as lit.
    # The cut-outs keep a 1.5-unit bridge between them so they never merge.
    fill = "#000000" if variant == "black" else "#FFFFFF"
    holes = circle_path(32, top, 4.5) + circle_path(32, mid, 7) + circle_path(32, bottom, 4.5)
    return f'<path d="{SHIELD} {holes}" fill="{fill}" fill-rule="evenodd"/>' + circle(32, mid, 4.5, fill)


# --- Wordmark ----------------------------------------------------------------

class Face:
    """One weight of the variable font, for shaping and outlines."""

    def __init__(self, path, weight):
        self.font = instantiateVariableFont(TTFont(path), {"wght": weight})
        self.glyphs = self.font.getGlyphSet()
        self.upm = self.font["head"].unitsPerEm
        self.cap = self.font["OS/2"].sCapHeight
        blob = hb.Blob.from_file_path(path)
        self.hb = hb.Font(hb.Face(blob))
        self.hb.set_variations({"wght": weight})

    def shape(self, text):
        buf = hb.Buffer()
        buf.add_str(text)
        buf.guess_segment_properties()
        hb.shape(self.hb, buf, {"kern": True, "liga": True})
        names = [self.font.getGlyphName(i.codepoint) for i in buf.glyph_infos]
        return list(zip(names, [p.x_advance for p in buf.glyph_positions]))


def text_paths(face, runs, size, x, baseline, tracking=-0.02):
    """Outline text. runs is [(text, colour)], shaped as one line so kerning
    holds across runs. Returns (svg, width, top, bottom) in output units."""
    s = size / face.upm
    track = tracking * face.upm
    out, pen_x = [], 0.0
    top, bottom = float("inf"), float("-inf")
    for text, colour in runs:
        d = []
        for name, advance in face.shape(text):
            pen = SVGPathPen(face.glyphs)
            face.glyphs[name].draw(TransformPen(pen, (s, 0, 0, -s, x + pen_x * s, baseline)))
            if pen.getCommands():
                d.append(pen.getCommands())
            bounds = BoundsPen(face.glyphs)
            face.glyphs[name].draw(bounds)
            if bounds.bounds:
                top = min(top, baseline - bounds.bounds[3] * s)
                bottom = max(bottom, baseline - bounds.bounds[1] * s)
            pen_x += advance + track
        out.append(f'<path d="{" ".join(d)}" fill="{colour}"/>')
    width = (pen_x - track) * s
    return "".join(out), width, top, bottom


WORD_COLOURS = {
    "color": (INK, AMBER_TEXT),
    "color-dark": (PAPER, AMBER),
    "black": ("#000000", "#000000"),
    "white": ("#FFFFFF", "#FFFFFF"),
}


def svg(width, height, body, title):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {width:.2f} {height:.2f}" '
            f'width="{width:.0f}" height="{height:.0f}" role="img" aria-label="{title}">'
            f"<title>{title}</title>{body}</svg>\n")


def horizontal(face, variant):
    kube, amber = WORD_COLOURS[variant]
    size, gap = 44, 14
    baseline = 32 + face.cap * size / face.upm / 2
    words, width, _, _ = text_paths(face, [("Kube", kube), ("Amber", amber)], size, 64 + gap, baseline)
    return svg(64 + gap + width, 64, icon(variant) + words, "KubeAmber")


def stacked(face, variant):
    kube, amber = WORD_COLOURS[variant]
    size, icon_size, gap = 44, 112, 18
    # Measure first, then centre both parts.
    _, width, top, bottom = text_paths(face, [("Kube", kube), ("Amber", amber)], size, 0, 0)
    total_w = max(width, icon_size)
    scale = icon_size / 64
    mark = f'<g transform="translate({(total_w - icon_size) / 2:.2f} 0) scale({scale})">{icon(variant)}</g>'
    baseline = icon_size + gap - top
    words, _, _, _ = text_paths(face, [("Kube", kube), ("Amber", amber)], size, (total_w - width) / 2, baseline)
    return svg(total_w, baseline + bottom, mark + words, "KubeAmber")


def icon_svg(variant):
    return svg(64, 64, icon(variant), "KubeAmber")


def social(bold, medium):
    """GitHub social preview, 1280 x 640, dark ground."""
    w, h = 1280, 640
    scale = 2.6
    mark_w = 64 * scale
    size, gap = 44 * scale, 14 * scale
    _, word_w, _, _ = text_paths(bold, [("Kube", PAPER), ("Amber", AMBER)], size, 0, 0)
    tag_size = 40
    _, tag_w, _, _ = text_paths(medium, [("Traffic lights for AI agents on Kubernetes", "#A8A29E")], tag_size, 0, 0, tracking=0)
    lock_w = mark_w + gap + word_w
    x0, y0 = (w - lock_w) / 2, 190
    baseline = y0 + 32 * scale + bold.cap * size / bold.upm / 2
    words, _, _, _ = text_paths(bold, [("Kube", PAPER), ("Amber", AMBER)], size, x0 + mark_w + gap, baseline)
    tag, _, _, _ = text_paths(medium, [("Traffic lights for AI agents on Kubernetes", "#A8A29E")], tag_size, (w - tag_w) / 2, 452, tracking=0)
    body = (f'<rect width="{w}" height="{h}" fill="#0C0A09"/>'
            f'<g transform="translate({x0:.2f} {y0}) scale({scale})">{icon("color-dark")}</g>{words}{tag}')
    return svg(w, h, body, "KubeAmber: traffic lights for AI agents on Kubernetes")


def avatar():
    """Square avatar for the GitHub organisation, 512 x 512."""
    scale = 6
    off = (512 - 64 * scale) / 2
    body = f'<rect width="512" height="512" fill="#F5F5F4"/><g transform="translate({off} {off}) scale({scale})">{icon("color")}</g>'
    return svg(512, 512, body, "KubeAmber")


def main():
    font_path, out = sys.argv[1], sys.argv[2]
    bold, medium = Face(font_path, 700), Face(font_path, 500)
    files = {}
    for variant in WORD_COLOURS:
        files[f"icon/{variant}/kubeamber-icon-{variant}.svg"] = icon_svg(variant)
        files[f"horizontal/{variant}/kubeamber-horizontal-{variant}.svg"] = horizontal(bold, variant)
        files[f"stacked/{variant}/kubeamber-stacked-{variant}.svg"] = stacked(bold, variant)
    files["social/kubeamber-social-preview.svg"] = social(bold, medium)
    files["avatar/kubeamber-avatar.svg"] = avatar()
    for rel, content in files.items():
        path = os.path.join(out, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            f.write(content)
        print(rel)


if __name__ == "__main__":
    main()
