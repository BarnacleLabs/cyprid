"""Generate cyprid's graphics: the logo mark, README banners and the social-preview image.

    python3 assets/generate.py

Writes SVGs next to this file. The social preview PNG (GitHub requires PNG or JPG)
is rendered from cyprid-social.svg with any browser or SVG renderer.
Standard library only.
"""
import os

HERE = os.path.dirname(os.path.abspath(__file__))

THEMES = {
    "dark": dict(
        bg0="#07131F", bg1="#0C2536", tile="#0B1B2B", tile_stroke="#24455E", text="#E6F2F7", muted="#86A0B3",
        card="#0F2436", card_stroke="#1D3A52", line="#24506B", bubble="#7DD3FC",
        feeler="#7DD3FC", node="#E0F2FE", pill="#102A3E", pill_text="#B9D3E1",
        accent="#2DD4BF", accent2="#38BDF8", bar_bg="#1A3549",
    ),
    "light": dict(
        bg0="#F5FBFD", bg1="#E3F3F7", tile="#0B1B2B", tile_stroke="#0B1B2B", text="#0B2233", muted="#557184",
        card="#FFFFFF", card_stroke="#CFE3EA", line="#A9CEDB", bubble="#0EA5E9",
        feeler="#7DD3FC", node="#E0F2FE", pill="#FFFFFF", pill_text="#28475A",
        accent="#0D9488", accent2="#0284C7", bar_bg="#E1EEF3",
    ),
}

SANS = "-apple-system, BlinkMacSystemFont, 'Segoe UI', Inter, Roboto, 'Helvetica Neue', Arial, sans-serif"
MONO = "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"


def mark(t, x, y, size, uid, tile=True):
    """The cyprid: an almond-shaped shell whose three feelers fan out to nodes (one request, many models)."""
    s = size / 128
    return f'''
  <g transform="translate({x} {y}) scale({s})">
    <defs><linearGradient id="shell{uid}" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#5EEAD4"/><stop offset=".55" stop-color="#22D3EE"/><stop offset="1" stop-color="#0EA5E9"/>
    </linearGradient></defs>
    {f'<rect x="1" y="1" width="126" height="126" rx="27" fill="{t["tile"]}" stroke="{t["tile_stroke"]}" stroke-width="2"/>' if tile else ""}
    <g transform="translate(6 2) rotate(-10 64 64)">
      <g fill="none" stroke="{t["feeler"]}" stroke-width="3.6" stroke-linecap="round">
        <path d="M36 62 C 28 54, 22 46, 18 38"/><path d="M35 64 C 26 64, 19 64, 13 64"/><path d="M36 66 C 28 74, 22 82, 18 90"/>
      </g>
      <g fill="{t["node"]}"><circle cx="17" cy="36" r="5.5"/><circle cx="11" cy="64" r="5.5"/><circle cx="17" cy="92" r="5.5"/></g>
      <path d="M34 64 C 44 44, 74 36, 96 42 C 110 46, 117 58, 113 71 C 109 83, 93 90, 71 88 C 53 86, 40 78, 34 64 Z" fill="url(#shell{uid})"/>
      <path d="M45 55 C 62 43, 90 42, 107 52" fill="none" stroke="#0B1B2B" stroke-opacity=".3" stroke-width="3" stroke-linecap="round"/>
      <circle cx="50" cy="63" r="5.2" fill="#0B1B2B"/><circle cx="51.8" cy="61.3" r="1.7" fill="#E0F2FE"/>
      <g fill="none" stroke="{t["feeler"]}" stroke-width="3" stroke-linecap="round">
        <path d="M74 88 C 76 95, 80 99, 86 101"/><path d="M85 87 C 88 93, 92 96, 98 97"/><path d="M96 84 C 99 89, 103 91, 108 91"/>
      </g>
    </g>
  </g>'''


def bubbles(t, w, h):
    spots = [(0.06, 0.86, 5), (0.11, 0.93, 3), (0.47, 0.12, 4), (0.52, 0.2, 2.5), (0.93, 0.1, 5),
             (0.97, 0.22, 3), (0.9, 0.9, 4), (0.58, 0.9, 3), (0.35, 0.95, 2.5)]
    return "".join(f'<circle cx="{w*a:.0f}" cy="{h*b:.0f}" r="{r}" fill="none" stroke="{t["bubble"]}" '
                   f'stroke-opacity=".28" stroke-width="1.5"/>' for a, b, r in spots)


def diagram(t, ox, oy, uid):
    """Request -> cyprid (routing to one of many models) -> typed answer."""
    hx, hy = ox + 260, oy + 200  # hub
    models = [("von", -95, -128), ("decider-4b", 0, -138), ("kev-4b", 95, -128),
              ("laya", -95, 128), ("openjev", 0, 138), ("cygnet", 95, 128)]
    out = []
    # model pills and their thin routing lines
    for i, (name, dx, dy) in enumerate(models):
        px, py = hx + dx, hy + dy
        active = name == "decider-4b"
        out.append(f'<path d="M{hx} {hy} L{px} {py}" stroke="{t["accent"] if active else t["line"]}" '
                   f'stroke-width="{2.5 if active else 1.5}" {"" if active else "stroke-dasharray=\"3 5\""} fill="none"/>')
    for name, dx, dy in models:
        px, py = hx + dx, hy + dy
        active = name == "decider-4b"
        w = 12 + 8.2 * len(name)
        fill = t["accent"] if active else t["pill"]
        stroke = t["accent"] if active else t["card_stroke"]
        color = "#FFFFFF" if active else t["pill_text"]
        out.append(f'<rect x="{px - w/2:.1f}" y="{py - 14}" width="{w:.1f}" height="28" rx="14" fill="{fill}" stroke="{stroke}"/>')
        out.append(f'<text x="{px}" y="{py + 5}" text-anchor="middle" font-family="{MONO}" font-size="13" '
                   f'font-weight="{600 if active else 500}" fill="{color}">{name}</text>')

    # request card
    rx, ry = ox, oy + 128
    out.append(f'''
    <path d="M{rx + 176} {hy} L{hx - 60} {hy}" stroke="url(#flow{uid})" stroke-width="3" fill="none" stroke-linecap="round"/>
    <rect x="{rx}" y="{ry}" width="176" height="144" rx="14" fill="{t["card"]}" stroke="{t["card_stroke"]}"/>
    <text x="{rx + 16}" y="{ry + 26}" font-family="{MONO}" font-size="12" fill="{t["muted"]}">POST /v1/systemone</text>
    <text x="{rx + 16}" y="{ry + 54}" font-family="{MONO}" font-size="13" fill="{t["accent2"]}">state</text>
    <text x="{rx + 16}" y="{ry + 74}" font-family="{SANS}" font-size="14" fill="{t["text"]}">"I was charged</text>
    <text x="{rx + 16}" y="{ry + 92}" font-family="{SANS}" font-size="14" fill="{t["text"]}"> twice. Help!"</text>
    <text x="{rx + 16}" y="{ry + 118}" font-family="{MONO}" font-size="13" fill="{t["accent2"]}">urgent? team? mood?</text>''')

    # hub
    out.append(f'''
    <circle cx="{hx}" cy="{hy}" r="62" fill="{t["accent"]}" fill-opacity=".10"/>
    <circle cx="{hx}" cy="{hy}" r="50" fill="{t["tile"]}" stroke="url(#flow{uid})" stroke-width="3"/>''')
    out.append(mark(t, hx - 40, hy - 40, 80, uid + "h", tile=False))  # the hub circle is its tile

    # answer card
    ax, ay = hx + 124, oy + 116
    out.append(f'''
    <path d="M{hx + 60} {hy} L{ax} {hy}" stroke="url(#flow{uid})" stroke-width="3" fill="none" stroke-linecap="round"/>
    <rect x="{ax}" y="{ay}" width="206" height="168" rx="14" fill="{t["card"]}" stroke="{t["card_stroke"]}"/>
    <text x="{ax + 16}" y="{ay + 26}" font-family="{MONO}" font-size="12" fill="{t["muted"]}">answers</text>''')
    rows = [("urgent", "yes", 0.69), ("team", "billing", 0.78), ("mood", "annoyed", 0.68)]
    for i, (q, a, p) in enumerate(rows):
        y = ay + 56 + i * 40
        out.append(f'''
    <text x="{ax + 16}" y="{y}" font-family="{MONO}" font-size="12" fill="{t["muted"]}">{q}</text>
    <text x="{ax + 190}" y="{y}" text-anchor="end" font-family="{SANS}" font-size="14" font-weight="600" fill="{t["text"]}">{a} {int(p*100)}%</text>
    <rect x="{ax + 16}" y="{y + 9}" width="174" height="7" rx="3.5" fill="{t["bar_bg"]}"/>
    <rect x="{ax + 16}" y="{y + 9}" width="{174*p:.0f}" height="7" rx="3.5" fill="url(#flow{uid})"/>''')
    return "".join(out)


def gradients(t, uid):
    return f'''
  <defs>
    <linearGradient id="bg{uid}" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="{t["bg0"]}"/><stop offset="1" stop-color="{t["bg1"]}"/>
    </linearGradient>
    <linearGradient id="flow{uid}" x1="0" y1="0" x2="1" y2="0">
      <stop offset="0" stop-color="{t["accent"]}"/><stop offset="1" stop-color="{t["accent2"]}"/>
    </linearGradient>
  </defs>'''


def title_block(t, x, y, uid, big=False):
    k = 1.15 if big else 1.0
    return f'''
  {mark(t, x, y, 132 * k, uid + "m")}
  <text x="{x + 156 * k:.0f}" y="{y + 92 * k:.0f}" font-family="{SANS}" font-size="{96 * k:.0f}" font-weight="800"
        letter-spacing="-3" fill="{t["text"]}">cyprid</text>
  <text x="{x:.0f}" y="{y + 192 * k:.0f}" font-family="{SANS}" font-size="{30 * k:.0f}" font-weight="650" fill="{t["text"]}">One API for many decision models</text>
  <text x="{x:.0f}" y="{y + 230 * k:.0f}" font-family="{SANS}" font-size="{21 * k:.0f}" fill="{t["muted"]}">Jev-compatible · install, run and compare them on your Mac</text>'''


def banner(theme):
    t, uid = THEMES[theme], theme
    w, h = 1280, 400
    return f'''<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" width="{w}" height="{h}" role="img" aria-label="cyprid: one API for many decision models">
  {gradients(t, uid)}
  <rect width="{w}" height="{h}" rx="24" fill="url(#bg{uid})"/>
  {bubbles(t, w, h)}
  {title_block(t, 64, 72, uid)}
  {diagram(t, 640, 0, uid)}
</svg>
'''


def social():
    t, uid = THEMES["dark"], "s"
    w, h = 1280, 640
    return f'''<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" width="{w}" height="{h}">
  {gradients(t, uid)}
  <rect width="{w}" height="{h}" fill="url(#bg{uid})"/>
  {bubbles(t, w, h)}
  {title_block(t, 64, 150, uid)}
  {diagram(t, 640, 120, uid)}
  <text x="64" y="578" font-family="{SANS}" font-size="20" fill="{t["muted"]}">Open source · runs locally on Apple Silicon Macs · von, decider, kev, laya, openjev, cygnet and more</text>
</svg>
'''


def logo():
    t = THEMES["dark"]
    return f'''<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128" width="128" height="128">{mark(t, 0, 0, 128, "l")}
</svg>
'''


if __name__ == "__main__":
    files = {"cyprid-banner-dark.svg": banner("dark"), "cyprid-banner-light.svg": banner("light"),
             "cyprid-social.svg": social(), "cyprid-logo.svg": logo()}
    for name, svg in files.items():
        with open(os.path.join(HERE, name), "w") as f:
            f.write(svg)
        print("wrote", name)
