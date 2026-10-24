# The slates

`loading.ts` is what a channel shows while its sources are first tried: the wordmark still, "Tuning in…", a
lit segment sweeping a bar every 2 s, over a warm breathing noise bed (`hum.py`) that swells with each
sweep. Made by `loading.py` and `hum.py` the same way as below (`tuning.png` is its caption).


`offair.ts` is what a channel plays while no source has video yet: 24 s of the bifrost wordmark
bouncing corner to corner, changing colour on each bounce, over a scrolling spectrum bar, with a
loading-screen tune (120 BPM, 12 bars: marimba, bass, shaker) synthesised by `music.py`. Picture and sound both end where they start, so the loop has no seam. bifrost embeds it
and plays it at its own pace; it costs no CPU.

To change it, edit `render.py` (picture) or `music.py` (sound) and rebuild it with a full ffmpeg:

```bash
cd slate
python3 render.py . | ffmpeg -y -f rawvideo -pix_fmt rgb24 -s 1280x720 -r 25 -i pipe:0 -t 24 \
  -c:v libx264 -preset veryslow -tune animation -crf 30 -g 50 -keyint_min 50 -sc_threshold 0 \
  -pix_fmt yuv420p -profile:v main -level 4.0 -x264-params repeat-headers=1 -an video.ts
python3 music.py music.wav
ffmpeg -y -i video.ts -i music.wav -map 0:v -map 1:a -c:v copy -c:a aac -b:a 96k -t 24 -f mpegts offair.ts
rm video.ts music.wav
```

`logo.png` and `text.png` are the wordmark (Unbounded 700) and the caption (Figtree 500) rendered
on a transparent background. Keep 1280x720, H.264 Main, AAC stereo 48 kHz: the formats the sources
use, so Jellyfin decides the same for the slate as for the broadcast.
