<p align="center">
  <img src="web/favicon.svg" width="96" alt="">
</p>

<h1 align="center">bifrost</h1>

<p align="center">
  <a href="https://github.com/PlatanosVerdes/bifrost/tags"><img src="https://img.shields.io/github/v/tag/PlatanosVerdes/bifrost?label=release&color=2f7d3a" alt="Latest release"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/PlatanosVerdes/bifrost" alt="Go version">
</p>

Reads an AceStream channel list published on IPFS, writes Jellyfin's M3U playlist and a cut-down
XMLTV guide, and serves `/channel/<slug>`: the proxy Jellyfin plays from.

```
IPFS list ──> bifrost (list loop) ──> channels_ace.m3u + guide.xml ──> Jellyfin
                     │ catalog
Jellyfin ──> bifrost (proxy) /channel/<slug> ──> aceserve /ace/getstream?id=<source> ──> ffmpeg -c copy
```

- **The list loop** reads the list every few minutes and rewrites Jellyfin's playlist and guide only
  when something really changed. Each channel has one stable URL however often its content ids rotate.
- **The proxy** opens a channel's sources on the AceStream engine (aceserve), remuxes each through
  `ffmpeg -c copy` and shares one engine session among everyone watching. When the playing source
  starves it switches to a better source of the same channel, and may step down to the channel's
  lower-resolution version and back up, telling the TV through Jellyfin. While nothing delivers it
  plays a loading slate, then an off-air slate.
- **The status page** shows what is playing and how well, lets you pick a channel's source, and can
  be shared through Cloudflare Access.
- **Metrics** go to a Pushgateway for Grafana.

It does not run the AceStream engine or a VPN; it talks to an engine over HTTP.

> The name: Bifröst is the rainbow bridge to Asgard. This one joins a P2P swarm to a Samsung TV.

## Running it

```yaml
bifrost:
  image: ghcr.io/platanosverdes/bifrost:${BIFROST_VERSION}
  environment:
    - SOURCE_URLS=http://kubo:8080/ipns/<key>/channels.m3u
    - ACESERVE_BASE_URL=http://172.19.0.1:6878
    - JELLYFIN_URL=http://jellyfin:8096
    - JELLYFIN_API_KEY=${JELLYFIN_API_KEY}
    - GUIDE_SOURCE_URL=http://lists.local/guide.xml
  volumes:
    - ./appdata/jellyfin/acestream:/data   # Jellyfin reads the M3U and guide.xml from here
    - ./appdata/bifrost:/state
```

Point Jellyfin's tuner at `/data/channels_ace.m3u` and its XMLTV provider at `/data/guide.xml`.

## Docs

| | |
| :--- | :--- |
| [Configuration](docs/configuration.md) | Every setting, the HTTP routes and the metrics |
| [Streams](docs/streams.md) | Swarm health, moving to a better source, resolution step-down, the slates, streams nobody watches |
| [Sharing](docs/sharing.md) | The page behind Cloudflare Access, and invite links |
| [Slates](slate/README.md) | How the two embedded slates are made and rebuilt |
| [Rules that must stay](docs/rules.md) | Things that look odd and were each learnt by breaking them. Read before changing the proxy |

## Development

```sh
go test -race ./...
```

Every push to `main` that touches code is tagged `vYYYY.MM.DD[.N]` and published as
`ghcr.io/platanosverdes/bifrost:<tag>` for arm64.
