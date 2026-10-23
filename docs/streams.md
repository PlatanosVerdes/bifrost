# Streams

What bifrost watches while a channel plays, and what it does about it.

## The swarm

A channel can open in a second and still stall every minute: the engine downloads slower than the
stream plays, its buffer runs dry, and the TV gives up and reopens. During a Champions League match
a 1080p channel of about 800 KB/s got 527–750 KB/s from its swarm, and nothing in bifrost said so.

So while a stream plays, bifrost asks the engine how its swarm is doing every 5 s. It does not open
anything to ask: aceserve redirects `/ace/getstream` to `/ace/r/<infohash>/<session>`, and
`/ace/stat/<infohash>/<session>` answers for that same playback. The engine does not say the
bitrate, so bifrost measures it: the bytes it relayed over the last minute, divided by how far the
engine's live position moved. Wall time would sink with the download exactly when it matters.

Each playing stream in `/api/status` carries a `swarm`:

| Field | What it is |
| :--- | :--- |
| `status` | The engine's state: `dl` while it downloads, `prebuf` while it refills its buffer |
| `peers` | Peers the engine is connected to |
| `download_bytes_per_second`, `upload_bytes_per_second` | The engine's rates (it counts in KiB/s; these are ×1024) |
| `bitrate_bytes_per_second` | The stream's rate, once ten seconds have played |
| `headroom` | Download over bitrate. Below 1 the buffer drains |
| `starving` | The download has been under the bitrate for three samples in a row (15 s) |
| `stalls` | Times the engine left `dl` since the stream started |
| `sampled_at`, `unavailable` | When the engine last answered, and why the last ask failed, if it did |

The page shows it in KB/s next to the channel (`12 peers · 850/820 KB/s`), red below 1, and the
log says when a channel starts and stops starving. The same numbers are pushed per channel slug
under their own job, `acestream_streams`, so the list loop's PUT and this one do not wipe each
other: `acestream_stream_viewers`, `_peers`, `_download_bytes_per_second`,
`_upload_bytes_per_second`, `_bitrate_bytes_per_second`, `_headroom`, `_starving`,
`_stalls_total` and `_source_switches_total` (since bifrost started), all labelled `channel="<slug>"`. A dashboard that filters on
`job="acestream_updater"` has to take in `acestream_streams` too to see them. A channel nobody
watches any more drops out of the group.

## Moving to a better source

A channel's sources do not degrade together. In the same match the playing source sat at
0.65–0.95x of its bitrate for minutes, and the TV cut out or lost its audio, while another source
of the same channel downloaded twice that. So a stream that keeps starving moves its viewers to a
better source of its channel, under rules meant never to leave them with nothing:

- **Only when it has starved for a while.** A probe starts once the stream has been starving (see
  above) for `SWITCH_AFTER`, so after about 45 s of a download under the bitrate, never while it
  keeps up.
- **One probe at a time, on the whole machine, and rarely.** The probe is a second engine session
  for the candidate's content id, next to the one playing. It lasts the open (at most 15 s) plus
  `PROBE_FOR`, and the channel waits `PROBE_COOLDOWN` after it before another. Candidates are
  taken in turn: one never probed first, else the one probed longest ago. The probe asks the engine
  with a `pid` of its own: the engine stops a player's previous download when it starts a new one,
  and without a pid a player is its address, so the first probe stopped the very stream it was
  meant to rescue.
- **Measured, not guessed.** The candidate is polled like the playing source, and its video is
  read and thrown away meanwhile, so the engine serves it as it would a player. Its first sample,
  the engine filling its opening buffer flat out, is left out; the rest still show more what its
  swarm can give than the rate it will settle at, which on a live stream is close to 1x. Its
  headroom is its download over the channel's measured bitrate, or its own if higher (sources of
  one channel can differ: one measured 470 KB/s, another 850), against the playing source's
  average over the last `SWITCH_AFTER`.
- **Clearly better or nothing.** It is switched to when it keeps up, at `SWITCH_MIN_HEADROOM` (1.1x)
  or more, and beats the playing source by `SWITCH_MARGIN` (0.2). When no source keeps up, the
  least bad one is taken only by twice that margin: 0.95x is worth leaving 0.5x for, not 0.7x. A
  candidate with fewer than half the playing source's peers needs one margin more. One that
  stalled in its last sample, gave fewer than two usable samples, or a playing source that
  recovered meanwhile, keeps the stream where it is.
- **The playing source is not touched until the new one delivers.** The candidate takes over
  between two chunks: the viewers stay connected and get the new source's packets from its first
  TS sync on. The relay hands out whole 188-byte packets, so the cut never falls inside one. The
  picture jumps (another source is at another point of the match), which beats a stall. Only then
  is the old session closed, and the new source becomes the channel's last good one.
- **A probe that fails changes nothing.** One that never opens, sends no video, ends, or is not
  better is closed, and the reason is logged and kept in the stream's `last_probe`.

The log says each step: `probing <id>` with the playing source's numbers, `[swarm] … probe <id>:
1.85x, 24 peers over 3 samples; playing <id>: 0.71x`, and then `switched <old> -> <new>, headroom
0.71x -> 1.85x` or `kept <id>: probe <id> not clearly better…`. Each playing stream in
`/api/status` carries:

| Field | What it is |
| :--- | :--- |
| `source` | The content id playing now |
| `probing`, `probing_source` | Whether another source is being measured, and which |
| `switches` | Times this stream moved to another source |
| `last_switch` | When, from and to which source, why, and the headroom before and after |
| `last_probe` | The last probe: when, which source, `switched` or `kept`, why, and both readings |

## Streams nobody watches

Jellyfin can lose track of a session and keep pulling a channel with nobody in front of it: a TV
cut off mid-stream (a bifrost restart) reopens the channel, and its later "stopped" is applied to the
old session, so the engine goes on downloading a match for nobody. Once a minute, while a stream is
open, bifrost asks Jellyfin which channels its sessions play. A stream whose every viewer is Jellyfin
(by User-Agent) and that no session has played for `ORPHAN_AFTER` in a row is let go, with a
`[proxy] … closed: no Jellyfin session has played it` line. It never closes on doubt: not while a
viewer that is not Jellyfin is on it, and not while Jellyfin cannot be asked. The `[proxy] …
requested` line names who opened a stream, `Jellyfin` or `direct: <User-Agent>`.

The 15 minutes are well past the five or six Jellyfin itself takes to notice a TV switched off
without a stop. A Jellyfin recording has no playing session either, so set it higher, or `0`, if
recordings are made from these channels.
