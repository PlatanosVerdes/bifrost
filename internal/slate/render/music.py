import math, random, struct, sys, wave
SR, BPM, BARS = 48000, 120, 12
BEAT = 60 / BPM
N = int(SR * BEAT * 4 * BARS)  # 24 s
L = [0.0] * N
R = [0.0] * N
def hz(m):
    return 440 * 2 ** ((m - 69) / 12)
def add(t0, samples, pan=0.5):
    i0 = int(t0 * SR)
    for k, v in enumerate(samples):
        i = (i0 + k) % N  # wraps round, so a tail rings into the start and the loop has no seam
        L[i] += v * (1 - pan)
        R[i] += v * pan
def marimba(m, dur=0.9, vol=0.22):
    f = hz(m)
    return [vol * math.exp(-t * 6) * min(1, t * 400) * (math.sin(2 * math.pi * f * t) + 0.35 * math.exp(-t * 18) * math.sin(2 * math.pi * 4 * f * t))
            for t in (n / SR for n in range(int(dur * SR)))]
def bass(m, dur, vol=0.32):
    f = hz(m)
    return [vol * math.exp(-t * 2.5) * min(1, t * 150) * (math.sin(2 * math.pi * f * t) + 0.25 * math.sin(4 * math.pi * f * t))
            for t in (n / SR for n in range(int(dur * SR)))]
def kick(vol=0.35):
    out, ph = [], 0.0
    for n in range(int(0.25 * SR)):
        t = n / SR
        ph += 2 * math.pi * (45 + 70 * math.exp(-t * 30)) / SR
        out.append(vol * math.exp(-t * 14) * math.sin(ph))
    return out
rng = random.Random(7)
def shaker(vol=0.05):
    out, prev = [], 0.0
    for n in range(int(0.07 * SR)):
        x = rng.uniform(-1, 1)
        prev = x - 0.85 * prev  # a crude high-pass, so it hisses rather than thumps
        out.append(vol * math.exp(-n / SR * 60) * prev)
    return out
def pad(notes, dur, vol=0.025):
    out = []
    for n in range(int(dur * SR)):
        t = n / SR
        env = min(1, t * 4) * min(1, (dur - t) * 4)
        out.append(vol * env * sum(math.sin(2 * math.pi * hz(m) * t) for m in notes))
    return out
# Cmaj7, Am7, Dm7, G7: one bar each, three times round.
CHORDS = [(48, [60, 64, 67, 71]), (45, [57, 60, 64, 67]), (50, [62, 65, 69, 72]), (43, [55, 59, 62, 65])]
# A two-bar motif in C major pentatonic, answered differently each time round. (beat, note, length)
MOTIF = [(0, 76, 0.5), (0.5, 79, 0.5), (1, 81, 1), (2, 79, 0.5), (2.5, 76, 0.5), (3, 74, 1),
         (4, 72, 0.5), (4.5, 74, 0.5), (5, 76, 1.5), (6.5, 72, 0.5), (7, 74, 1)]
ANSWERS = [[(0, 72, 0.5), (0.5, 74, 0.5), (1, 76, 1), (2, 79, 1), (3, 76, 1), (4, 74, 2), (6, 72, 2)],
           [(0, 81, 0.5), (0.5, 79, 0.5), (1, 76, 1), (2, 74, 0.5), (2.5, 76, 0.5), (3, 79, 1), (4, 76, 2), (6, 74, 2)],
           [(0, 76, 0.5), (0.5, 74, 0.5), (1, 72, 1), (2, 74, 1), (3, 79, 1), (4, 72, 4)]]
for bar in range(BARS):
    t = bar * 4 * BEAT
    root, notes = CHORDS[bar % 4]
    add(t, pad(notes, 4 * BEAT), 0.5)
    for b, m in ((0, root), (1.5, root + 7), (2, root + 12), (3, root + 7)):
        add(t + b * BEAT, bass(m, 0.45 if b != 2 else 0.9), 0.5)
    for b in (0, 2):
        add(t + b * BEAT, kick(), 0.5)
    for e in range(8):
        add(t + e * BEAT / 2, shaker(0.05 if e % 2 else 0.03), 0.65)
for cycle in range(3):
    t = cycle * 16 * BEAT
    for b, m, d in MOTIF:
        add(t + b * BEAT, marimba(m, 0.9), 0.4)
    for b, m, d in ANSWERS[cycle]:
        add(t + (8 + b) * BEAT, marimba(m, 0.9), 0.4)
peak = max(max(map(abs, L)), max(map(abs, R)))
scale = 0.5 / peak
with wave.open(sys.argv[1], "wb") as w:
    w.setnchannels(2); w.setsampwidth(2); w.setframerate(SR)
    w.writeframes(b"".join(struct.pack("<hh", int(L[i] * scale * 32767), int(R[i] * scale * 32767)) for i in range(N)))
