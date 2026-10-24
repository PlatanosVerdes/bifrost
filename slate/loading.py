import sys
from PIL import Image, ImageDraw
W, H, FPS, T = 1280, 720, 25, 24
BG = (13, 16, 32)
d = sys.argv[1]
full = Image.open(f"{d}/tuning.png").convert("RGBA")
text = full.crop(full.getbbox())
logo = Image.open(f"{d}/logo.png").convert("RGBA")
ink = Image.new("RGBA", logo.size, (0xe9, 0xeb, 0xf5, 255))
ink.putalpha(logo.getchannel("A"))
base = Image.new("RGB", (W, H), BG)
lx, ly = (W - logo.width) // 2, H // 2 - logo.height - 10
base.paste(ink, (lx, ly), ink)
tx, ty = (W - text.width) // 2, ly + logo.height + 36
base.paste(text, (tx, ty), text)
BW, BH, PERIOD = 360, 6, 2.0
SPECTRUM = [(0xe4,0x47,0x5b),(0xf0,0x8a,0x3e),(0xe7,0xc1,0x3f),(0x3f,0xb2,0x7f),(0x3f,0x8f,0xe0),(0x7a,0x5c,0xe6)]
TRACK = (0x25, 0x2b, 0x44)
bx, by = (W - BW) // 2, ty + text.height + 22
mask = Image.new("L", (BW, BH), 0)
ImageDraw.Draw(mask).rounded_rectangle((0, 0, BW - 1, BH - 1), radius=BH // 2, fill=255)
def spectrum(u):
    u = min(max(u, 0), 0.9999) * (len(SPECTRUM) - 1)
    i = int(u); f = u - i
    a, b = SPECTRUM[i], SPECTRUM[i + 1]
    return tuple(round(a[k] + (b[k] - a[k]) * f) for k in range(3))
def bar(t):
    # A lit segment a third of the bar long sweeps across it, entering and leaving at the edges.
    img = Image.new("RGB", (BW, BH), TRACK)
    px = img.load()
    seg = BW / 3
    head = (t % PERIOD) / PERIOD * (BW + seg)
    for x in range(BW):
        if head - seg <= x <= head:
            fade = min(1, (x - (head - seg)) / (seg * 0.4))
            c = spectrum(x / BW)
            c = tuple(round(TRACK[k] + (c[k] - TRACK[k]) * fade) for k in range(3))
            for y in range(BH):
                px[x, y] = c
    return img
out = sys.stdout.buffer
for f in range(FPS * T):
    frame = base.copy()
    frame.paste(bar(f / FPS), (bx, by), mask)
    out.write(frame.tobytes())
