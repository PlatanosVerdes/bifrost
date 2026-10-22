# bifrost

Bridges an AceStream channel list published on IPFS to Jellyfin's Live TV.

> **Why the name.** In Norse myth, Bifröst is the burning rainbow bridge between Midgard and Asgard,
> guarded day and night by Heimdall, who can hear grass grow and wool grow on sheep. Here it joins a
> peer-to-peer swarm of dubious provenance to a Samsung TV, guarded by a Raspberry Pi that can play
> exactly one channel at a time. The scale differs; the job is the same. It also sits next to a
> server called streamhalla, and once you have named one machine after Valhalla, the bridge to it
> was never going to be called `acestream-gateway`.
>
> The myth ends with the bridge collapsing under the weight of the giants at Ragnarök. The real
> giants are 34 MB guides re-parsed every few minutes and two connections to one channel, which is
> why most of this README is about not letting them cross.

One process, two halves sharing the channel catalog in memory:

- **The list loop** reads the list every 1–5 minutes, folds its sources into channels, and writes
  Jellyfin's M3U and a guide cut down to those channels. It tells Jellyfin to refresh only when one
  of them really changed.
- **The proxy** is what Jellyfin plays from. For each channel it tries the sources one at a time
  until one produces video, and shares that single engine connection among everyone watching.

It does not run the AceStream engine or any VPN. It talks to an engine (`aceserve`) over HTTP, and
how that engine reaches the internet is the host's business (see [The engine and the VPN](#the-engine-and-the-vpn)).

```
IPFS list ──> bifrost (list loop) ──> channels_ace.m3u + guide.xml ──> Jellyfin
                     │ catalog
Jellyfin ──> bifrost (proxy) /channel/<slug> ──> aceserve /ace/getstream?id=<source>
```

## Running it

```yaml
bifrost:
  image: ghcr.io/platanosverdes/bifrost:${BIFROST_VERSION}
  environment:
    - SOURCE_URLS=http://kubo:8080/ipns/<key>/hashes_acestream.m3u
    - ACESERVE_BASE_URL=http://172.19.0.1:6878
    - CHANNEL_BASE_URL=http://bifrost:6879/channel/
    - GUIDE_SOURCE_URL=https://raw.githubusercontent.com/davidmuma/EPG_dobleM/refs/heads/master/guiatv.xml
    - JELLYFIN_URL=http://jellyfin:8096
    - JELLYFIN_API_KEY=${JELLYFIN_API_KEY}
    - PUSHGATEWAY_URL=http://pushgateway:9091
  volumes:
    - ./appdata/jellyfin/acestream:/data   # Jellyfin reads the M3U and guide.xml from here
    - ./appdata/bifrost:/state
```

| Variable | Default | What it is |
| :--- | :--- | :--- |
| `SOURCE_URLS` | required | Comma-separated routes to the list; the first that answers wins |
| `ACESERVE_BASE_URL` | `http://aceserve:6878` | The engine's HTTP API |
| `CHANNEL_BASE_URL` | `http://bifrost:6879/channel/` | How Jellyfin reaches this proxy; written into the M3U |
| `GUIDE_SOURCE_URL` | empty (no guide) | Full XMLTV guide, cut down into `guide.xml` |
| `JELLYFIN_URL`, `JELLYFIN_API_KEY` | required | To run Refresh Guide |
| `PUSHGATEWAY_URL` | empty | Where the `acestream_*` metrics go |
| `OUTPUT_FILE` | `/data/channels_ace.m3u` | Jellyfin's playlist; `guide.xml` is written next to it |
| `STATE_DIR` | `/state` | Run counters, the cached full guide, and `invites.json` (who invited each viewer, which Cloudflare cannot tell) |
| `SOURCE_WAIT` / `CHANNEL_MAX_WAIT` | `25s` / `90s` | Per source, and in all, before a channel is given up |
| `MAX_WAIT`, `LINGER` | `45s`, `10s` | The legacy `?id=` route's wait; how long a stream outlives its last viewer |
| `BIFROST_NAME` | the hostname | The machine's name, shown on the status page |
| `SWITCH_AFTER` | `30s` | How long a stream starves before another source is probed; `0` turns switching off |
| `PROBE_FOR`, `PROBE_COOLDOWN` | `20s`, `3m` | How long a probed source is measured; the wait between two probes of a channel |
| `ORPHAN_AFTER` | `15m` | Let go a stream only Jellyfin holds once no Jellyfin session has played it for this long; `0` turns it off. Needs `JELLYFIN_URL` |
| `SWITCH_MIN_HEADROOM`, `SWITCH_MARGIN` | `1.1`, `0.2` | What a probed source needs to be switched to (see [Moving to a better source](#moving-to-a-better-source)) |
| `STARVING_BELOW` | `1` | A sample is short when the download is under this times the bitrate. Only raised to test switching |
| `ACCESS_TEAM_DOMAIN` | empty (sharing off) | The Cloudflare Access team in front of bifrost, `<team>.cloudflareaccess.com`. See [Sharing](#sharing) |
| `ACCESS_AUD` | empty (sharing off) | This bifrost's Access app's tag: Access app → Overview → Application Audience (AUD) Tag. Tokens for any other app are refused |
| `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN` | empty (sharing off) | The account that holds the Access app, and an account-owned token with only Access: Apps Read, Access: Policies Read and Access: Groups Edit |
| `BIFROST_ADMINS` | empty (nobody) | Comma-separated emails that may change who has access from the page |

Jellyfin's tuner points at `/data/channels_ace.m3u` and its XMLTV provider at `/data/guide.xml`
(as Jellyfin sees those paths).

### HTTP

| Route | What it answers |
| :--- | :--- |
| `GET /` | The status page: every channel with its state (playing, last open played, left early, failed, not opened yet) and its sources first, then an Activity tab with what is playing, recent opens, and the list and guide. Reads the two API routes below every 10 s |
| `GET /channel/<slug>` | The channel's stream, from the first source that produces video |
| `GET /ace/getstream?id=<id>` | One source, as the engine would serve it (shared the same way) |
| `GET /api/channels` | Every channel with its group, logo, tvg-id and sources (the one that last played first), what is playing on it now, its last open since bifrost started, and what it airs now and next by the guide |
| `GET /api/status` | The last list read and change, the last Jellyfin refresh, what is playing (channel, source, viewers, since, seconds until the engine's first video, the programme on air and the next one, from guide.xml, its `swarm`, and whether it is probing another source, how often it switched and its last switch and probe, see below), the last 20 opens with their outcome, and how Jellyfin is playing each live channel (which user on which device and client, copy or transcode, with Jellyfin's reasons, and its speed against live: ffmpeg's frame rate over the channel's, below 1 it stalls), asked from its /Sessions on each request |
| `GET /api/access` | Whether sharing is on, and who is looking (`me`, `admin`). For an admin, also the link to send and the lists of people the Access app lets in, each with its members and whether bifrost may edit it |
| `POST`, `DELETE /api/access/members` | Add addresses to a list (`{"group", "emails"}`) or take one off (`{"group", "email"}`). Admins only; answers what `GET /api/access` does |
| `GET /healthz` | 200 once a list has been read |

None of them opens a channel: `/api/status` and `/api/channels` only report what bifrost already
knows from the list and from the streams passing through it. A player that goes to the engine
directly (and so skips the shared connection) can take a channel's current source from
`/api/channels`.

The metrics keep the names of the service this replaced (`acestream_run_total`,
`acestream_unique_channels`, `acestream_jellyfin_refresh_http_code`…), pushed under the job
`acestream_updater`, so existing dashboards and alerts keep working.

### Streams nobody watches

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

### Sharing

The page can be opened from outside the house through a [Cloudflare Access](https://developers.cloudflare.com/cloudflare-one/policies/access/)
app in front of bifrost, one app per bifrost. With `ACCESS_TEAM_DOMAIN`, `ACCESS_AUD`,
`CLOUDFLARE_ACCOUNT_ID` and `CLOUDFLARE_API_TOKEN` set, the page shows who is signed in, and an admin (`BIFROST_ADMINS`) gets a
Share… button to invite people by email and remove them.

bifrost needs no names. It reads who is looking only from the JWT Access signs into
`Cf-Access-Jwt-Assertion`, checked against the team's keys, never from a header or a host name a
proxy can change. The JWT's `aud` must be `ACCESS_AUD`, the tag on the app's Overview page under
Application Audience (AUD) Tag: bifrost is also reachable without Cloudflare (on the LAN or a
tailnet), where a token the same team issued for another app could otherwise be replayed. The tag
finds the app in the account; the link is the app's domain, and the lists are the Access groups its allow policies include, inline or reusable. A group
another Access app also uses, or one nested in another group, is shown but never edited, since
changing it would change who opens that app too; so is a group one of this app's deny or bypass
policies names. While any policy or rule in the account cannot be read, no group is edited at all. Editing a group only adds or removes its `email` rules; any other rule in it (a whole
domain, a login method) stays as it is. bifrost never creates or deletes groups, policies or apps:
make the app, a policy, and a group of its own for it in Cloudflare once, and manage the people
from the page.

### The swarm

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

### Moving to a better source

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

## Rules that look odd and must stay

Each one was learnt by breaking it.

**Nothing opens a channel on its own.** No health checks, no warm-up, no probing at refresh time.
A Raspberry Pi plays one channel; a second connection to the engine saturates it, and a probe of a
channel someone is watching takes the engine away from them. Sources are tried only when someone
opens the channel, one after another, never two at once. The one exception is a stream that has
been starving for a while: it may measure one other source of its channel at a time, for a bounded
time, rarely (see [Moving to a better source](#moving-to-a-better-source)).

**One engine connection per channel, shared.** Jellyfin opens every live channel twice at once, one
connection to play and one to probe the codecs. The engine serves a channel to one connection, so
the probe got a 500, Jellyfin decided without knowing the codecs, and re-encoded a plain H.264
stream with libx264, which a Pi 4 does at a fraction of real time: the picture played a few seconds
and stalled. With the probe answered, Jellyfin copies the stream and the Pi barely notices.

**Whole 32 KB chunks.** A viewer is dropped once it falls 256 chunks behind. The engine hands data
over a few KB at a time, and counting those as chunks made the backlog a few hundred KB: the opening
burst filled it and dropped the TV the moment the channel started.

**One URL per channel, never one per source.** Jellyfin keys a channel on the MD5 of its URL. The
list republishes the same channels under new content ids several times an hour, and an id in the URL
made each rotation a new channel, a deleted old one, and a guide refresh. The catalog maps
`/channel/<slug>` to the ids of the moment; a rotation changes the catalog and nothing in Jellyfin.

**The catalog is replaced whole every pass.** An id the list dropped is gone at once. The only thing
kept over is a channel someone is watching right now, so Jellyfin does not delete it under them.

**Each open gets a connection of its own.** The engine answers its redirects with no length and
keeps the connection open, so a redirect sent over a connection kept alive from an earlier request
(the swarm's stat polls) waits for a body that never ends. Opens to the engine never reuse one.

**Refresh Guide is found by its key.** Task ids are an MD5 of the class name. The hard-coded id this
replaced was Refresh Channels (internet channels), which Jellyfin accepted with a 204 and which never
reread the playlist, for six months.

**The guide is cut down to our channels.** The published guide is 34 MB for 642 channels; ours use
about 22 of them and 2% of the programmes. Parsing all of it was what made a refresh cost a Pi 4 two
and a half minutes. Cut down it is under 1 MB and a refresh takes seconds, so it can follow every
real change. The full guide is downloaded once a day; a new channel re-trims from that copy.

**One refresh per burst of changes.** Jellyfin is refreshed two minutes after the last change, so a
channel that drops out and comes back in consecutive passes costs one refresh, not two.

## The engine and the VPN

bifrost only needs HTTP to the engine. On a line where the ISP intercepts the AceStream API during
matches (Spain, with a self-signed certificate on api.acestream.me), only the engine's HTTPS should
go through a VPN, with its P2P on the home line: an engine entirely behind a VPN gets no inbound
peers, and channels that start in 2 seconds took 30, or not at all. With gluetun:

```yaml
gluetun:
  ports: ["127.0.0.1:8888:8888"]
  environment: [HTTPPROXY=on, HTTPPROXY_LISTENING_ADDRESS=:8888, FIREWALL_INPUT_PORTS=8888]
aceserve:
  network_mode: host
  environment:
    - https_proxy=http://127.0.0.1:8888   # the engine is Python; urllib honours it
    - no_proxy=localhost,127.0.0.1
```

An engine on a bridge network resolves names itself against public DNS and cannot find `gluetun` by
name: point `https_proxy` at an address, such as the network's gateway.

## Development

```sh
go test -race ./...
```

Every push to `main` is tagged with CalVer (`vYYYY.MM.DD`, then `.1`, `.2`…) and published as
`ghcr.io/platanosverdes/bifrost:<tag>` for arm64, through the shared release workflow in
[PlatanosVerdes/workflows](https://github.com/PlatanosVerdes/workflows).
