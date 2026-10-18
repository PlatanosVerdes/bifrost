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
| `STATE_DIR` | `/state` | Run counters and the cached full guide |
| `SOURCE_WAIT` / `CHANNEL_MAX_WAIT` | `25s` / `90s` | Per source, and in all, before a channel is given up |
| `MAX_WAIT`, `LINGER` | `45s`, `10s` | The legacy `?id=` route's wait; how long a stream outlives its last viewer |
| `BIFROST_NAME` | the hostname | The machine's name, shown on the status page |

Jellyfin's tuner points at `/data/channels_ace.m3u` and its XMLTV provider at `/data/guide.xml`
(as Jellyfin sees those paths).

### HTTP

| Route | What it answers |
| :--- | :--- |
| `GET /` | The status page: every channel with its state (playing, last open played, left early, failed, not opened yet) and its sources first, then an Activity tab with what is playing, recent opens, and the list and guide. Reads the two API routes below every 10 s |
| `GET /channel/<slug>` | The channel's stream, from the first source that produces video |
| `GET /ace/getstream?id=<id>` | One source, as the engine would serve it (shared the same way) |
| `GET /api/channels` | Every channel with its group, logo, tvg-id and sources (the one that last played first), what is playing on it now, and its last open since bifrost started |
| `GET /api/status` | The last list read and change, the last Jellyfin refresh, what is playing (channel, source, viewers, since, seconds until the engine's first video), the last 20 opens with their outcome, and how Jellyfin is playing each live channel (copy or transcode, with Jellyfin's reasons, and its speed against live: ffmpeg's frame rate over the channel's, below 1 it stalls), asked from its /Sessions on each request |
| `GET /healthz` | 200 once a list has been read |

None of them opens a channel: `/api/status` and `/api/channels` only report what bifrost already
knows from the list and from the streams passing through it. A player that goes to the engine
directly (and so skips the shared connection) can take a channel's current source from
`/api/channels`.

The metrics keep the names of the service this replaced (`acestream_run_total`,
`acestream_unique_channels`, `acestream_jellyfin_refresh_http_code`…), pushed under the job
`acestream_updater`, so existing dashboards and alerts keep working.

## Rules that look odd and must stay

Each one was learnt by breaking it.

**Nothing opens a channel on its own.** No health checks, no warm-up, no probing at refresh time.
A Raspberry Pi plays one channel; a second connection to the engine saturates it, and a probe of a
channel someone is watching takes the engine away from them. Sources are tried only when someone
opens the channel, one after another, never two at once.

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
