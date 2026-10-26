# Configuration

| Variable | Default | What it is |
| :--- | :--- | :--- |
| `SOURCE_URLS` | required | The channel list: comma-separated routes to it, the first that answers wins. Several lists as JSON, see below |
| `KEEP_DROPPED` | `72h` | How long a channel or source the list dropped stays in the playlist (see [Rules](rules.md#the-list-drops-what-still-plays)). `0` plays the list exactly as it is, for a list already curated upstream that removes channels on purpose |
| `ACESERVE_BASE_URL` | `http://aceserve:6878` | The engine's HTTP API |
| `CHANNEL_BASE_URL` | `http://bifrost:6879/channel/` | How Jellyfin reaches this proxy; written into the M3U |
| `GUIDE_SOURCE_URL` | empty (no guide) | Full XMLTV guide, cut down into `guide.xml` |
| `JELLYFIN_URL`, `JELLYFIN_API_KEY` | required | To run Refresh Guide |
| `PUSHGATEWAY_URL` | empty | Where the `acestream_*` metrics go |
| `OUTPUT_FILE` | `/data/channels_ace.m3u` | Jellyfin's playlist; `guide.xml` is written next to it |
| `STATE_DIR` | `/state` | Run counters, the cached full guide, `sources.json` (what was last seen of each source, so a restart still tries a lately failed one last; a week unseen is dropped), `remembered.json` (every channel and source the list has carried, see [Rules](rules.md#the-list-drops-what-still-plays)), and `invites.json` (who invited each viewer, which Cloudflare cannot tell) and `join-links.json` (invite links not used yet, by hash) |
| `SOURCE_WAIT` / `CHANNEL_MAX_WAIT` | `25s` / `90s` | Per source, and in all, before a channel is given up |
| `MAX_WAIT` | `45s` | How long the `?id=` route (one content id, no fallback) waits for video |
| `LINGER` | `10s` | How long a stream outlives its last viewer |
| `BIFROST_NAME` | the hostname | The machine's name, shown on the status page |
| `SWITCH_BEHIND` | `5s` | How much video the playing source may owe before another source is probed; `0` turns switching off |
| `PROBE_FOR`, `PROBE_COOLDOWN` | `20s`, `3m` | How long a probed source is measured; the wait between two probes of a channel |
| `ORPHAN_AFTER` | `15m` | Let go a stream only Jellyfin holds once no Jellyfin session has played it for this long; `0` turns it off. Needs `JELLYFIN_URL` |
| `SWITCH_MIN_HEADROOM`, `SWITCH_MARGIN` | `1.1`, `0.2` | What a probed source needs to be switched to (see [Moving to a better source](streams.md#moving-to-a-better-source)) |
| `FFMPEG` | `ffmpeg` | The ffmpeg that remuxes every source so a switch keeps one clock (the image ships a minimal one); `off` relays sources raw |
| `SLATE` | `on` | The loading and off-air slates played while a channel has no video; `off` plays none: the viewer waits for video or gets a 503. Needs ffmpeg |
| `RESOLUTION_SWITCH` | `on` | A starving channel may step down to its lower version (same guide id, name differing only in `720p`/`1080p`) and back up; `off` keeps every channel on its own sources |
| `STARVING_BELOW` | `1` | A sample is short when the download is under this times the bitrate. Only raised to test switching |
| `ACCESS_TEAM_DOMAIN` | empty (sharing off) | The Cloudflare Access team in front of bifrost, `<team>.cloudflareaccess.com`. See [Sharing](sharing.md) |
| `ACCESS_AUD` | empty (sharing off) | This bifrost's Access app's tag: Access app → Overview → Application Audience (AUD) Tag. Tokens for any other app are refused |
| `ACCESS_JOIN_AUD` | empty (no invite links) | The AUD tag of a second Access app on `<domain>/join/*` that lets anyone sign in. See [Invite links](sharing.md#invite-links) |
| `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN` | empty (sharing off) | The account that holds the Access app, and an account-owned token with only Access: Apps Read, Access: Policies Read and Access: Groups Edit |
| `BIFROST_ADMINS` | empty (nobody) | Comma-separated emails that may change who has access from the page. They can also make members admins from the Share dialog (kept in `STATE_DIR/admins.json`); the ones listed here cannot be changed from the page |

Jellyfin's tuner points at `/data/channels_ace.m3u` and its XMLTV provider at `/data/guide.xml`
(as Jellyfin sees those paths).

## Several lists

`SOURCE_URLS` takes a JSON array for more than one list, each with optional backup routes to the
same list. It can also be the path of a file holding that array (it starts with `/`), reread on
every pass so an edit applies without a restart:

Lines starting with `//` are skipped, so a list can be kept in the file switched off, ready to
come back with one edit and no restart.

```json
[
  {"url": "http://kubo:8080/ipns/<key>/list.m3u", "backup": ["https://ipfs.io/ipns/<key>/list.m3u"]},
  {"url": "http://lists.local/other.m3u"}
]
```

Their sources add up: a channel gets the sources every list has for it, the first list's first. A
later list's entry joins a channel by its name, or by the same guide id and resolution when it names
it otherwise; a content id in two lists counts once. Tags (logo, guide id, group) come from the list
that brought the channel. A list none of whose routes answers is read from its last copy in
`STATE_DIR/lists`, so it drops nothing.

## HTTP

| Route | What it answers |
| :--- | :--- |
| `GET /` | The status page: every channel with its state (playing, last open played, left early, failed, not opened yet) and its sources first, then an Activity tab with what is playing, recent opens, and the list and guide. Reads the two API routes below every 10 s |
| `GET /channel/<slug>` | The channel's stream, from the first source that produces video |
| `GET /ace/getstream?id=<id>` | One source, as the engine would serve it (shared the same way) |
| `GET /api/channels` | Every channel with its group, logo, tvg-id and sources (the one that last played first), what is playing on it now, its last open since bifrost started, and what it airs now and next by the guide. `sources` lists them in the playlist's order with what was last seen of each: height and audio from the remux, headroom and peers, when, and whether it played, was probed or failed |
| `POST /api/channels/<slug>/source` | `{"id": "<content id>"}`: moves a playing channel to another of its own sources, as a switch does. One switch at a time on the machine; the automatic switch then waits its cooldown. 404 for an id of another channel, 409 when it is not playing or another switch runs, 502 when the source did not start (the stream stays where it was) |
| `GET /api/status` | The last list read and change, the last Jellyfin refresh, what is playing (channel, source, viewers, since, seconds until the engine's first video, the programme on air and the next one, from guide.xml, its `swarm`, and whether it is probing another source, how often it switched and its last switch and probe, see below), the last 20 opens with their outcome, and how Jellyfin is playing each live channel (which user on which device and client, copy or transcode, with Jellyfin's reasons, and its speed against live: ffmpeg's frame rate over the channel's, below 1 it stalls), asked from its /Sessions on each request |
| `GET /api/access` | Whether sharing is on, and who is looking (`me`, `admin`). For an admin, also the link to send and the lists of people the Access app lets in, each with its members and whether bifrost may edit it |
| `POST`, `DELETE /api/access/members` | Add addresses to a list (`{"group", "emails"}`) or take one off (`{"group", "email"}`). Admins only; answers what `GET /api/access` does |
| `POST`, `DELETE /api/access/links` | Make an invite link for a list (`{"group"}`, answers it once as `created`) or take one back (`{"id"}`). Admins only |
| `GET /join/<token>` | Adds whoever opens an invite link to its list, once, and sends them to the page |
| `GET /healthz` | 200 once a list has been read |

None of them opens a channel: `/api/status` and `/api/channels` only report what bifrost already
knows from the list and from the streams passing through it. A player that goes to the engine
directly (and so skips the shared connection) can take a channel's current source from
`/api/channels`.

## Metrics

Pushed to `PUSHGATEWAY_URL` with PUT, so a series that disappears is removed. Two jobs, so the two
PUTs do not wipe each other:

- `acestream_updater`, per list pass: `acestream_run_total`, `acestream_updates_with_changes_total`,
  `acestream_updates_no_changes_total`, `acestream_update_errors_total`, `acestream_last_run_timestamp`,
  `acestream_unique_channels`, `acestream_jellyfin_refresh_http_code`,
  `acestream_jellyfin_last_refresh_timestamp` and `acestream_source_http_code{url}`. They keep the
  names of the service this replaced, so existing dashboards and alerts keep working.
- `acestream_streams`, every 5 s for the playing channels, labelled `channel="<slug>"`:
  `acestream_stream_viewers`, `_peers`, `_download_bytes_per_second`, `_upload_bytes_per_second`,
  `_bitrate_bytes_per_second`, `_headroom`, `_starving`, and the counters `_stalls_total`,
  `_source_switches_total` and `_remux_restarts_total`. See [Streams](streams.md#the-swarm).
