# The slates

bifrost embeds two 24 s MPEG-TS loops and plays them itself, so a slate costs no CPU:

- `loading.ts`: shown while a channel's sources are first tried. The wordmark still, "Tuning in…" and a
  lit segment sweeping a bar every 2 s, over a warm noise bed that swells and pans left to right with
  each sweep. Made by `loading.py` (picture, with `tuning.png` as the caption) and `hum.py` (sound).
- `offair.ts`: shown while no source has video. The wordmark bouncing corner to corner, changing colour
  on each bounce, over a scrolling spectrum bar, with a 120 BPM, 12-bar loading-screen tune (marimba,
  bass, shaker). Made by `render.py` (picture, with `logo.png` and `text.png`) and `music.py` (sound).

Picture and sound both end where they start, so the loop has no seam. Both tracks are exactly 24.000 s:
600 video frames at 25 fps and 1125 AAC frames. The audio is trimmed to 1150976 samples before encoding
(1124 frames of 1024 plus the encoder's priming frame) and muxed by copy.

## Rebuilding

Needs Python 3 with Pillow and an ffmpeg with libx264 and the native AAC encoder. From `slate/`, set
`PICTURE`/`SOUND` to `render.py`/`music.py` for the off-air slate or `loading.py`/`hum.py` for the loading
one, and `OUT` to `offair.ts` or `loading.ts`:

```bash
python3 $PICTURE . | ffmpeg -y -f rawvideo -pix_fmt rgb24 -s 1280x720 -r 25 -i pipe:0 -t 24 \
  -c:v libx264 -preset veryslow -tune animation -crf 30 -g 50 -keyint_min 50 -sc_threshold 0 \
  -pix_fmt yuv420p -profile:v main -level 4.0 -x264-params repeat-headers=1 -an video.ts
python3 $SOUND sound.wav
ffmpeg -y -i sound.wav -af atrim=end_sample=1150976 -c:a aac -b:a 96k -f adts audio.aac
ffmpeg -y -i video.ts -i audio.aac -map 0:v -map 1:a -c copy -f mpegts $OUT
rm video.ts sound.wav audio.aac
```

Check the result with `ffprobe -count_frames`: 600 video frames, 1125 audio frames.

`logo.png` is the wordmark (Unbounded 700) and `text.png` the off-air caption (Figtree 500), both on a
transparent background. Keep 1280x720, H.264 Main, AAC stereo 48 kHz: the formats the sources use, so
Jellyfin decides the same for a slate as for the broadcast.
