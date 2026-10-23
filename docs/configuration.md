# Configuration

| Variable | Default | What it is |
| :--- | :--- | :--- |
| `SOURCE_URLS` | required | Comma-separated routes to the list; the first that answers wins |
| `ACESERVE_BASE_URL` | `http://aceserve:6878` | The engine's HTTP API |
| `CHANNEL_BASE_URL` | `http://bifrost:6879/channel/` | How Jellyfin reaches this proxy; written into the M3U |
| `GUIDE_SOURCE_URL` | empty (no guide) | Full XMLTV guide, cut down into `guide.xml` |
| `JELLYFIN_URL`, `JELLYFIN_API_KEY` | required | To run Refresh Guide |
| `PUSHGATEWAY_URL` | empty | Where the `acestream_*` metrics go |
| `OUTPUT_FILE` | `/data/channels_ace.m3u` | Jellyfin's playlist; `guide.xml` is written next to it |
| `STATE_DIR` | `/state` | Run counters, the cached full guide, and `invites.json` (who invited each viewer, which Cloudflare cannot tell) and `join-links.json` (invite links not used yet, by hash) |
| `SOURCE_WAIT` / `CHANNEL_MAX_WAIT` | `25s` / `90s` | Per source, and in all, before a channel is given up |
| `MAX_WAIT`, `LINGER` | `45s`, `10s` | The legacy `?id=` route's wait; how long a stream outlives its last viewer |
| `BIFROST_NAME` | the hostname | The machine's name, shown on the status page |
| `SWITCH_AFTER` | `30s` | How long a stream starves before another source is probed; `0` turns switching off |
| `PROBE_FOR`, `PROBE_COOLDOWN` | `20s`, `3m` | How long a probed source is measured; the wait between two probes of a channel |
| `ORPHAN_AFTER` | `15m` | Let go a stream only Jellyfin holds once no Jellyfin session has played it for this long; `0` turns it off. Needs `JELLYFIN_URL` |
| `SWITCH_MIN_HEADROOM`, `SWITCH_MARGIN` | `1.1`, `0.2` | What a probed source needs to be switched to (see [Moving to a better source](streams.md#moving-to-a-better-source)) |
| `STARVING_BELOW` | `1` | A sample is short when the download is under this times the bitrate. Only raised to test switching |
| `ACCESS_TEAM_DOMAIN` | empty (sharing off) | The Cloudflare Access team in front of bifrost, `<team>.cloudflareaccess.com`. See [Sharing](sharing.md) |
| `ACCESS_AUD` | empty (sharing off) | This bifrost's Access app's tag: Access app → Overview → Application Audience (AUD) Tag. Tokens for any other app are refused |
| `ACCESS_JOIN_AUD` | empty (no invite links) | The AUD tag of a second Access app on `<domain>/join/*` that lets anyone sign in. See [Invite links](sharing.md#invite-links) |
| `CLOUDFLARE_ACCOUNT_ID`, `CLOUDFLARE_API_TOKEN` | empty (sharing off) | The account that holds the Access app, and an account-owned token with only Access: Apps Read, Access: Policies Read and Access: Groups Edit |
| `BIFROST_ADMINS` | empty (nobody) | Comma-separated emails that may change who has access from the page |

Jellyfin's tuner points at `/data/channels_ace.m3u` and its XMLTV provider at `/data/guide.xml`
(as Jellyfin sees those paths).

## HTTP

| Route | What it answers |
| :--- | :--- |
| `GET /` | The status page: every channel with its state (playing, last open played, left early, failed, not opened yet) and its sources first, then an Activity tab with what is playing, recent opens, and the list and guide. Reads the two API routes below every 10 s |
| `GET /channel/<slug>` | The channel's stream, from the first source that produces video |
| `GET /ace/getstream?id=<id>` | One source, as the engine would serve it (shared the same way) |
| `GET /api/channels` | Every channel with its group, logo, tvg-id and sources (the one that last played first), what is playing on it now, its last open since bifrost started, and what it airs now and next by the guide |
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

The metrics keep the names of the service this replaced (`acestream_run_total`,
`acestream_unique_channels`, `acestream_jellyfin_refresh_http_code`…), pushed under the job
`acestream_updater`, so existing dashboards and alerts keep working.
