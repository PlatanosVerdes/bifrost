# Rules that look odd and must stay

Each one was learnt by breaking it.

**Nothing opens a channel on its own.** No health checks, no warm-up, no probing at refresh time.
A Raspberry Pi plays one channel; a second connection to the engine saturates it, and a probe of a
channel someone is watching takes the engine away from them. Sources are tried only when someone
opens the channel, one after another, never two at once. The one exception is a stream that has
been starving for a while: it may measure one other source of its channel at a time, for a bounded
time, rarely (see [Moving to a better source](streams.md#moving-to-a-better-source)).

**One engine connection per channel, shared.** Jellyfin opens every live channel twice at once, one
connection to play and one to probe the codecs. The engine serves a channel to one connection, so
the probe got a 500, Jellyfin decided without knowing the codecs, and re-encoded a plain H.264
stream with libx264, which a Pi 4 does at a fraction of real time: the picture played a few seconds
and stalled. With the probe answered, Jellyfin copies the stream and the Pi barely notices.

**A HEAD never opens a channel.** Jellyfin sends HEAD to tell HLS from TS, then GET on the same
connection. A HEAD that streamed would hold that connection and leave the GET waiting until Jellyfin's
100 s timeout, so HEAD answers `video/mp2t` and nothing else.

**Every source goes through its own `ffmpeg -c copy`.** Each source has its own clock, PIDs and GOP,
and splicing one in raw froze the Samsung TV. The remuxed runs are rebased into one continuous
stream, and the old source is closed only once the new one has delivered (see
[Moving to a better source](streams.md#moving-to-a-better-source)). The slates are spliced the same way.

**The probe asks the engine with a `pid` of its own.** The engine tells players apart by address and
a new download stops that player's previous one, so a probe without a pid killed the stream it was
meant to rescue.

**Whole 32 KB chunks.** A viewer is dropped once it falls 1024 chunks behind, about 30 s. The engine
hands data over a few KB at a time, and counting those as chunks made the backlog a few hundred KB:
the opening burst filled it and dropped the TV the moment the channel started. At 256 (about 9 s), a
Jellyfin guide refresh on a loaded Pi stalled its reading long enough to drop the TV mid-match.

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
about 23 of them and 2% of the programmes. Parsing all of it was what made a refresh cost a Pi 4 two
and a half minutes. Cut down it is under 1 MB and a refresh takes seconds, so it can follow every
real change. The full guide is downloaded once a day; a new channel re-trims from that copy.

**One refresh per burst of changes.** Jellyfin is refreshed two minutes after the last change, so a
channel that drops out and comes back in consecutive passes costs one refresh, not two.

## The engine and the VPN

bifrost only needs HTTP to the engine. On a line where the ISP blocks the AceStream API and the
engine's content servers (a self-signed certificate on api.acestream.me, a block page over plain
HTTP), only the engine's HTTP and HTTPS should go through a VPN, with its P2P on the home line: an
engine entirely behind a VPN gets no inbound peers, and channels that start in 2 seconds took 30, or
not at all. With HTTPS alone through the VPN, ids the engine has seen before still open and new ones
fail with `failed to load content`. With gluetun:

```yaml
gluetun:
  ports: ["127.0.0.1:8888:8888"]
  environment: [HTTPPROXY=on, HTTPPROXY_LISTENING_ADDRESS=:8888, FIREWALL_INPUT_PORTS=8888]
aceserve:
  network_mode: host
  environment:
    - http_proxy=http://127.0.0.1:8888    # the engine is Python; urllib honours both
    - https_proxy=http://127.0.0.1:8888
    - no_proxy=localhost,127.0.0.1
```

An engine on a bridge network resolves names itself against public DNS and cannot find `gluetun` by
name: point the proxies at an address, such as the network's gateway.
