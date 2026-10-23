<p align="center">
  <img src="web/favicon.svg" width="96" alt="">
</p>

<h1 align="center">bifrost</h1>

<p align="center">
  <a href="https://github.com/PlatanosVerdes/bifrost/tags"><img src="https://img.shields.io/github/v/tag/PlatanosVerdes/bifrost?label=release&color=2f7d3a" alt="Latest release"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/PlatanosVerdes/bifrost" alt="Go version">
</p>

Bridges an AceStream channel list published on IPFS to Jellyfin's Live TV: stable channel URLs, a
live source picked when someone opens a channel, and a guide cut down to the channels you have.

```
IPFS list ──> bifrost (list loop) ──> channels_ace.m3u + guide.xml ──> Jellyfin
                     │ catalog
Jellyfin ──> bifrost (proxy) /channel/<slug> ──> aceserve /ace/getstream?id=<source>
```

- **The list loop** reads the list every few minutes and rewrites Jellyfin's playlist and guide only
  when something really changed.
- **The proxy** tries a channel's sources one at a time until one plays, and shares that single
  engine connection with everyone watching.
- **The status page** shows what is playing and how well, and can be shared through Cloudflare Access.

It does not run the AceStream engine or a VPN; it talks to an engine (`aceserve`) over HTTP.

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
| [Streams](docs/streams.md) | Swarm health, moving to a better source, streams nobody watches |
| [Sharing](docs/sharing.md) | The page behind Cloudflare Access, and invite links |
| [Rules that must stay](docs/rules.md) | Things that look odd and were each learnt by breaking them. Read before changing the proxy |

## Development

```sh
go test -race ./...
```

Every push to `main` that touches code is tagged `vYYYY.MM.DD[.N]` and published as
`ghcr.io/platanosverdes/bifrost:<tag>` for arm64.
