import math, random, struct, sys, wave
SR, T, PERIOD = 48000, 24, 2.0
N = SR * T
rng = random.Random(3)
white = [rng.uniform(-1, 1) for _ in range(N)]
# Two one-pole low-passes run twice round the buffer: the second lap starts in the state the first
# ended in, so the warm noise is periodic and the loop has no seam.
lp1 = lp2 = 0.0
noise = [0.0] * N
for lap in range(2):
    for n in range(N):
        lp1 += 0.06 * (white[n] - lp1)
        lp2 += 0.06 * (lp1 - lp2)
        noise[n] = lp2
CHORD = [48, 55, 62, 64, 71]  # Cmaj9, low and open
def hz(m):
    return 440 * 2 ** ((m - 69) / 12)
L, R = [], []
for n in range(N):
    t = n / SR
    swell = 0.35 + 0.65 * math.sin(math.pi * (t % PERIOD) / PERIOD) ** 2  # rises with each bar sweep
    pad = sum(math.sin(2 * math.pi * hz(m) * t + k) for k, m in enumerate(CHORD)) / len(CHORD)
    pan = 0.15 * math.sin(2 * math.pi * t / PERIOD)
    L.append(1.6 * noise[n] * swell * (1 - pan) + 0.12 * pad)
    R.append(1.6 * noise[n] * swell * (1 + pan) + 0.12 * pad)
peak = max(max(map(abs, L)), max(map(abs, R)))
scale = 0.25 / peak
with wave.open(sys.argv[1], "wb") as w:
    w.setnchannels(2); w.setsampwidth(2); w.setframerate(SR)
    w.writeframes(b"".join(struct.pack("<hh", int(L[i] * scale * 32767), int(R[i] * scale * 32767)) for i in range(N)))
