import sys
from PIL import Image
W, H, FPS, T = 1280, 720, 25, 24
BG = (13, 16, 32)
COLORS = ["#e4475b", "#f08a3e", "#e7c13f", "#3fb27f", "#3f8fe0", "#7a5ce6", "#8f93ff", "#e9ebf5"]
d = sys.argv[1]
logo = Image.open(f"{d}/logo.png").convert("RGBA")
text = Image.open(f"{d}/text.png").convert("RGBA")
lw, lh = logo.size
M = 24
xmin, xmax = M, W - M - lw
ymin, ymax = M, H - M - lh - 110
kx, ky = 2, 3
def tri(u):
    u %= 2
    return u if u < 1 else 2 - u
events = sorted({round(i * T / (2 * kx), 6) for i in range(1, 2 * kx + 1)} | {round(i * T / (2 * ky), 6) for i in range(1, 2 * ky + 1)})
assert len(events) == 8, events
tints = []
for c in COLORS:
    r, g, b = int(c[1:3], 16), int(c[3:5], 16), int(c[5:7], 16)
    solid = Image.new("RGBA", logo.size, (r, g, b, 255))
    solid.putalpha(logo.getchannel("A"))
    tints.append(solid)
base = Image.new("RGB", (W, H), BG)
tx, ty = (W - text.width) // 2, H - M - 70
base.paste(text, (tx, ty), text)
BW, BH, PERIOD = 240, 4, 3.0
SPECTRUM = [(0xe4,0x47,0x5b),(0xf0,0x8a,0x3e),(0xe7,0xc1,0x3f),(0x3f,0xb2,0x7f),(0x3f,0x8f,0xe0),(0x7a,0x5c,0xe6)]
bx, by = (W - BW) // 2, ty + text.height + 16
mask = Image.new("L", (BW, BH), 0)
from PIL import ImageDraw
ImageDraw.Draw(mask).rounded_rectangle((0, 0, BW - 1, BH - 1), radius=BH // 2, fill=255)
def spectrum(u):
    u = (u % 1) * len(SPECTRUM)
    i = int(u); f = u - i
    a, b = SPECTRUM[i], SPECTRUM[(i + 1) % len(SPECTRUM)]
    return tuple(round(a[k] + (b[k] - a[k]) * f) for k in range(3))
def bar(t):
    img = Image.new("RGB", (BW, BH))
    px = img.load()
    for x in range(BW):
        c = spectrum(x / BW - t / PERIOD)
        for y in range(BH):
            px[x, y] = c
    return img
out = sys.stdout.buffer
for f in range(FPS * T):
    t = f / FPS
    x = xmin + (xmax - xmin) * tri(t * 2 * kx / T)
    y = ymin + (ymax - ymin) * tri(t * 2 * ky / T)
    idx = sum(1 for e in events if 1e-9 < e <= t + 1e-9) % len(COLORS)
    frame = base.copy()
    frame.paste(tints[idx], (round(x), round(y)), tints[idx])
    frame.paste(bar(t), (bx, by), mask)
    out.write(frame.tobytes())
