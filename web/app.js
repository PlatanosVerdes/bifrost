// bifrost's status page: Lit components, one class per piece. The app element fetches, everything
// below it renders what it is given. Nothing here opens a channel.
import { LitElement, html, css, nothing } from "./vendor/lit-all.min.js";

const REFRESH_MS = 10000;

// --- formatting, shared by every component ---------------------------------------------------

const known = (t) => { const d = new Date(t); return !isNaN(d) && d.getFullYear() > 2000 ? d : null; };
const hhmm = (t) => known(t)?.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) ?? "";
const ago = (t) => {
  const d = known(t);
  if (!d) return "never";
  const s = Math.max(0, Math.round((Date.now() - d) / 1000));
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  return `${Math.floor(s / 3600)} h ${Math.round((s % 3600) / 60)} min ago`;
};
const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;
const secs = (n) => `${Number(n ?? 0).toFixed(1)} s`;

// What a channel's dot and line say: playing now, else its last open since bifrost started.
function channelState(channel, jellyfin) {
  if (channel.playing) {
    const jf = jellyfin.find((p) => p.channel === channel.name);
    const how = jf ? (jf.video === "copy" ? " · Jellyfin copies" : " · Jellyfin re-encodes") : "";
    return { tone: "live", text: `Playing · ${channel.playing.viewers} watching · first video in ${secs(channel.playing.started_after_s)}${how}` };
  }
  const o = channel.last_open;
  if (!o) return { tone: "none", text: "Not opened yet" };
  if (o.outcome === "playing") return { tone: "ok", text: `Played at ${hhmm(o.at)} · first video in ${secs(o.seconds)}` };
  if (o.outcome === "gave_up") return { tone: "bad", text: `No source played at ${hhmm(o.at)} (${secs(o.seconds)})` };
  return { tone: "warn", text: `Left before it started at ${hhmm(o.at)} (${secs(o.seconds)})` };
}

// Styles every component starts from: cards, dots, small text. Colours only through the tokens.
const base = css`
  :host { display: block; }
  * { box-sizing: border-box; }
  .card { background: var(--surface); border: 1px solid var(--line); border-radius: 14px; padding: 18px; min-width: 0; }
  .muted { color: var(--muted); }
  .mono { font-family: var(--font-data); font-variant-numeric: tabular-nums; }
  .dot { width: 9px; height: 9px; border-radius: 50%; flex: none; display: inline-block; }
  .dot.live { background: var(--accent); box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent) 25%, transparent); }
  .dot.ok { background: var(--ok); } .dot.bad { background: var(--bad); } .dot.warn { background: var(--warn); }
  .dot.none { background: transparent; border: 1.5px solid var(--line); }
  .ok { color: var(--ok); } .bad { color: var(--bad); } .warn { color: var(--warn); } .live { color: var(--accent); }
  h2 { font-size: 13px; text-transform: uppercase; letter-spacing: .09em; color: var(--muted); margin: 26px 0 10px; font-weight: 700; }
  h2.first { margin-top: 0; }
`;

// --- one block of channels, scrolling on its own ---------------------------------------------

class ChannelGroup extends LitElement {
  static properties = { name: {}, channels: { attribute: false }, jellyfin: { attribute: false } };
  static styles = [base, css`
    .card { height: 300px; display: flex; flex-direction: column; }
    h3 { margin: 0 0 6px; font-size: 12px; letter-spacing: .08em; text-transform: uppercase; color: var(--muted); display: flex; justify-content: space-between; gap: 8px; }
    h3 span { font-family: var(--font-data); letter-spacing: 0; text-transform: none; font-weight: 500; }
    ul { list-style: none; margin: 0; padding: 0 4px 0 0; overflow-y: auto; min-height: 0; flex: 1; scrollbar-width: thin; }
    li { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; gap: 2px 10px; align-items: center; padding: 8px 0; border-top: 1px solid var(--line); }
    li:first-child { border-top: 0; }
    .name { font-weight: 600; font-size: 14px; min-width: 0; }
    .state { grid-column: 2 / 4; font-size: 12px; color: var(--muted); }
    .src { font-family: var(--font-data); font-size: 12px; color: var(--muted); white-space: nowrap; }
  `];

  render() {
    return html`<section class="card">
      <h3>${this.name}<span>${this.channels.length}</span></h3>
      <ul>${this.channels.map((c) => {
        const st = channelState(c, this.jellyfin);
        return html`<li>
          <i class="dot ${st.tone}" aria-hidden="true"></i>
          <span class="name">${c.name}</span>
          <span class="src">${plural(c.ids.length, "source")}</span>
          <span class="state ${st.tone === "none" ? "" : st.tone}">${st.text}</span>
        </li>`;
      })}</ul>
    </section>`;
  }
}
customElements.define("channel-group", ChannelGroup);

// --- the Channels tab: search, legend, the groups ---------------------------------------------

class ChannelBoard extends LitElement {
  static properties = { channels: { attribute: false }, jellyfin: { attribute: false }, query: { state: true } };
  static styles = [base, css`
    .tools { display: flex; flex-wrap: wrap; gap: 8px 14px; align-items: center; margin-bottom: 14px; }
    input { font: inherit; font-size: 14px; border: 1px solid var(--line); background: var(--surface); color: var(--ink); border-radius: 10px; padding: 8px 12px; width: min(100%, 300px); }
    input:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .legend { display: flex; flex-wrap: wrap; gap: 6px 14px; font-size: 12px; color: var(--muted); }
    .legend span { display: inline-flex; align-items: center; gap: 6px; }
    .groups { display: grid; gap: 14px; grid-template-columns: repeat(auto-fill, minmax(min(100%, 320px), 1fr)); }
    .empty { color: var(--muted); font-size: 14px; margin: 0; }
  `];

  constructor() { super(); this.channels = []; this.jellyfin = []; this.query = ""; }

  groups() {
    const needle = this.query.trim().toLowerCase();
    const out = [], at = new Map();
    for (const c of this.channels) {
      const group = c.group || "Other";
      if (needle && !c.name.toLowerCase().includes(needle) && !group.toLowerCase().includes(needle)) continue;
      if (!at.has(group)) { at.set(group, out.length); out.push({ name: group, channels: [] }); }
      out[at.get(group)].channels.push(c);
    }
    return out;
  }

  render() {
    const groups = this.groups();
    return html`
      <div class="tools">
        <input type="search" placeholder="Search channels…" aria-label="Search channels" .value=${this.query} @input=${(e) => { this.query = e.target.value; }}>
        <div class="legend">
          <span><i class="dot live"></i>Playing</span><span><i class="dot ok"></i>Last open played</span>
          <span><i class="dot warn"></i>Last open left early</span><span><i class="dot bad"></i>Last open failed</span>
          <span><i class="dot none"></i>Not opened yet</span>
        </div>
      </div>
      ${groups.length
        ? html`<div class="groups">${groups.map((g) => html`<channel-group .name=${g.name} .channels=${g.channels} .jellyfin=${this.jellyfin}></channel-group>`)}</div>`
        : html`<p class="empty">${this.channels.length ? "No channel matches." : "No channel list yet."}</p>`}`;
  }
}
customElements.define("channel-board", ChannelBoard);

// --- the Activity tab: now, recent opens, list and guide ---------------------------------------

class ActivityView extends LitElement {
  static properties = { status: { attribute: false } };
  static styles = [base, css`
    .row { display: grid; grid-template-columns: auto 1fr; gap: 14px; align-items: center; }
    .row + .row { margin-top: 12px; }
    .ring { width: 44px; height: 44px; border-radius: 50%; border: 2px dashed var(--line); display: grid; place-items: center; color: var(--muted); }
    .ring.on { border: 2px solid var(--accent); color: var(--accent); }
    .row p { margin: 2px 0 0; color: var(--muted); font-size: 14px; max-width: 62ch; }
    .duo { display: grid; gap: 14px; grid-template-columns: repeat(auto-fit, minmax(min(100%, 260px), 1fr)); }
    .big { display: flex; gap: 22px; flex-wrap: wrap; margin-bottom: 10px; }
    .big b { display: block; font-family: var(--font-display); font-weight: 500; font-size: 28px; line-height: 1.1; font-variant-numeric: tabular-nums; }
    .big span { color: var(--muted); font-size: 13px; }
    dl { display: grid; grid-template-columns: auto 1fr; gap: 6px 14px; margin: 0; font-size: 14px; }
    dt { color: var(--muted); } dd { margin: 0; min-width: 0; }
    ul { list-style: none; margin: 0; padding: 0; font-size: 13px; }
    li { display: grid; grid-template-columns: 46px minmax(0, 1fr) auto; gap: 1px 10px; padding: 7px 0; border-top: 1px solid var(--line); align-items: baseline; }
    li:first-child { border-top: 0; padding-top: 0; }
    .at { font-family: var(--font-data); font-size: 12px; color: var(--muted); }
    .ch { font-weight: 600; min-width: 0; }
    .res { font-family: var(--font-data); font-size: 12px; white-space: nowrap; }
    .tag { font-family: var(--font-data); font-size: 11px; border-radius: 5px; padding: 0 6px; margin-left: 6px; vertical-align: 1px; }
    .tag.copy { background: color-mix(in srgb, var(--ok) 16%, transparent); color: var(--ok); }
    .tag.trans { background: color-mix(in srgb, var(--warn) 18%, transparent); color: var(--warn); }
    .tag.slow { background: color-mix(in srgb, var(--bad) 16%, transparent); color: var(--bad); }
  `];

  screen(on) {
    return html`<div class="ring ${on ? "on" : ""}" aria-hidden="true"><svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><rect x="3" y="5" width="18" height="12" rx="2"/><path d="M8 21h8"/></svg></div>`;
  }

  now() {
    const streams = this.status.streams || [], jellyfin = this.status.jellyfin_playing || [];
    if (!streams.length) {
      return html`<div class="row">${this.screen(false)}<div><b>Nobody is watching</b>
        <p>When a channel is open it shows here: its source, how many are watching, how soon the engine gave the first video, whether Jellyfin copies the video or re-encodes it, and whether it keeps up with live.</p></div></div>`;
    }
    return streams.map((st) => {
      const jf = jellyfin.find((p) => st.channel.startsWith(p.channel));
      return html`<div class="row">${this.screen(true)}<div>
        <b>${st.channel}</b>${jf ? html`<span class="tag ${jf.video === "copy" ? "copy" : "trans"}">${jf.video === "copy" ? "copy" : "re-encode"}</span>` : nothing}${jf?.speed ? html`<span class="tag ${jf.speed >= 0.95 ? "copy" : "slow"}" title="Jellyfin's speed against live: below 1x it falls behind and the picture stalls">${jf.speed.toFixed(2)}x</span>` : nothing}
        <p>Source ${(st.source || "").slice(0, 8)} · ${st.viewers} watching · since ${hhmm(st.since)} · first video in ${secs(st.started_after_s)}${jf?.video !== "copy" && jf?.reasons ? ` · Jellyfin: ${jf.reasons.join(", ")}` : ""}</p>
      </div></div>`;
    });
  }

  recent() {
    const recent = this.status.recent || [];
    if (!recent.length) return html`<li><span></span><span class="muted">No opens since bifrost started.</span><span></span></li>`;
    return recent.map((r) => {
      const tone = r.outcome === "playing" ? "ok" : r.outcome === "gave_up" ? "bad" : "warn";
      const res = r.outcome === "playing" ? secs(r.seconds) : r.outcome === "gave_up" ? "no video" : "left early";
      return html`<li><span class="at">${hhmm(r.at)}</span><span class="ch">${r.channel}</span><span class="res ${tone}">${res}</span></li>`;
    });
  }

  render() {
    if (!this.status) return nothing;
    const l = this.status.list || {}, g = this.status.guide || {}, j = this.status.jellyfin || {};
    return html`
      <h2 class="first">Now</h2>
      <section class="card">${this.now()}</section>
      <div class="duo">
        <div><h2>Recent opens</h2><section class="card"><ul>${this.recent()}</ul></section></div>
        <div><h2>List and guide</h2><section class="card">
          <div class="big"><div><b>${l.channels ?? "–"}</b><span>channels</span></div><div><b>${l.sources ?? "–"}</b><span>sources</span></div><div><b>${g.channels ?? "–"}</b><span>with guide</span></div></div>
          <dl>
            <dt>List</dt><dd>read ${ago(l.last_read)}${known(l.last_change) ? `; changed ${ago(l.last_change)}` : ""}${l.error ? html` · <span class="bad">${l.error}</span>` : nothing}</dd>
            <dt>Guide</dt><dd>${g.bytes ? `${Math.round(g.bytes / 1024)} KB for ${g.channels} channels` : "not cut yet"}</dd>
            <dt>Jellyfin</dt>${j.last_code
              ? html`<dd class="${j.last_code === 204 ? "ok" : "bad"}">reread the guide ${ago(j.last_refresh)} (${j.last_code})</dd>`
              : html`<dd>no refresh asked yet</dd>`}
            ${j.refresh_due ? html`<dt>Next</dt><dd>refresh due at ${hhmm(j.refresh_due)}</dd>` : nothing}
          </dl>
        </section></div>
      </div>`;
  }
}
customElements.define("activity-view", ActivityView);

// --- the page: header, tabs, fetching ---------------------------------------------------------

class BifrostApp extends LitElement {
  static properties = { status: { state: true }, channels: { state: true }, view: { state: true }, reachable: { state: true } };
  static styles = [base, css`
    .wrap { max-width: 1080px; margin: 0 auto; padding-block: 22px 48px;
      padding-left: max(16px, env(safe-area-inset-left, 0px)); padding-right: max(16px, env(safe-area-inset-right, 0px)); }
    header { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 10px 16px; }
    .brand { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; }
    .word { font-family: var(--font-display); font-weight: 700; font-size: 30px; letter-spacing: -.02em; margin: 0; }
    .right { display: flex; gap: 10px; flex-wrap: wrap; align-items: center; }
    .chip { font-family: var(--font-data); font-size: 12px; background: var(--chip); border-radius: 6px; padding: 2px 8px; }
    .pill { display: inline-flex; align-items: center; gap: 6px; font-size: 13px; font-weight: 600; }
    .band { height: 4px; border-radius: 4px; background: var(--spectrum); margin: 14px 0 16px; }
    .bar { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 18px; }
    nav { display: flex; gap: 4px; background: var(--surface); border: 1px solid var(--line); border-radius: 999px; padding: 3px; }
    nav a { color: var(--muted); text-decoration: none; font-size: 13px; font-weight: 600; padding: 6px 14px; border-radius: 999px; }
    nav a[aria-current="page"] { background: var(--ink); color: var(--bg); }
    nav a:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .summary { display: flex; flex-wrap: wrap; gap: 6px 18px; font-size: 13px; color: var(--muted); }
    .summary b { color: var(--ink); font-family: var(--font-data); font-weight: 500; }
    .note { color: var(--muted); font-size: 12px; margin-top: 28px; max-width: 72ch; }
  `];

  constructor() {
    super();
    this.status = null; this.channels = []; this.reachable = true;
    this.view = location.hash === "#activity" ? "activity" : "channels";
  }

  connectedCallback() {
    super.connectedCallback();
    this.onHash = () => { this.view = location.hash === "#activity" ? "activity" : "channels"; };
    window.addEventListener("hashchange", this.onHash);
    this.refresh();
    this.timer = setInterval(() => this.refresh(), REFRESH_MS);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    window.removeEventListener("hashchange", this.onHash);
    clearInterval(this.timer);
  }

  async refresh() {
    try {
      const [status, channels] = await Promise.all([
        fetch("api/status").then((r) => r.json()), fetch("api/channels").then((r) => r.json())]);
      this.status = status; this.channels = channels.channels || []; this.reachable = true;
      document.title = `bifrost · ${status.name || ""}`;
    } catch {
      this.reachable = false;
    }
  }

  render() {
    const s = this.status, l = s?.list || {};
    const healthy = this.reachable && (l.channels ?? 0) > 0;
    const watching = (s?.streams || []).reduce((a, st) => a + st.viewers, 0);
    const tab = (id, label) => html`<a href="#${id}" aria-current=${this.view === id ? "page" : nothing}>${label}</a>`;
    return html`<div class="wrap">
      <header>
        <div class="brand"><p class="word">bifrost</p><span class="muted">${s?.name ?? ""} · ${location.host}</span></div>
        <div class="right">
          <span class="chip">${s?.version ?? "…"}</span>
          <span class="pill"><span class="dot ${healthy ? "ok" : "bad"}"></span>${!this.reachable ? "Not answering" : healthy ? "Healthy" : s ? "No list yet" : "Loading"}</span>
        </div>
      </header>
      <div class="band" aria-hidden="true"></div>
      <div class="bar">
        <nav aria-label="Views">${tab("channels", "Channels")}${tab("activity", "Activity")}</nav>
        <div class="summary">
          <span><b>${l.channels ?? 0}</b> channels</span><span><b>${l.sources ?? 0}</b> sources</span>
          <span>list read <b>${ago(l.last_read)}</b></span><span>${watching ? html`<b>${watching}</b> watching` : "nobody watching"}</span>
        </div>
      </div>
      ${this.view === "activity"
        ? html`<activity-view .status=${s}></activity-view>`
        : html`<channel-board .channels=${this.channels} .jellyfin=${s?.jellyfin_playing || []}></channel-board>`}
      <p class="note">Nothing on this page opens a channel: states come from real opens since bifrost last started, and from what Jellyfin reports about its own sessions. A channel with several sources skips a dead one by itself. Refreshes every 10 s.</p>
    </div>`;
  }
}
customElements.define("bifrost-app", BifrostApp);
