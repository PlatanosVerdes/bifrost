// bifrost's status page: the app element fetches, every component below renders what it is given.
import { LitElement, html, css, nothing } from "./vendor/lit-all.min.js";

const REFRESH_MS = 3000;

// Drawn here from the email, so the address never leaves the page: two hues from its hash and the
// first letter of the user part.
const avatars = new Map();
const nobody = "data:image/svg+xml," + encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" fill="#8b90a7"><circle cx="16" cy="12.5" r="6.5"/><ellipse cx="16" cy="31" rx="12" ry="10"/></svg>');
const fnv = (s) => { let h = 2166136261; for (const c of s) { h ^= c.codePointAt(0); h = Math.imul(h, 16777619); } return h >>> 0; };
const gradient = (email) => {
  const h = fnv(email), a = h % 360, b = (a + 40 + (h >>> 9) % 80) % 360;
  const letter = (email.split("@")[0].match(/[\p{L}\p{N}]/u)?.[0] ?? "?").toLocaleUpperCase();
  return "data:image/svg+xml," + encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">`
    + `<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="hsl(${a} 75% 62%)"/><stop offset="1" stop-color="hsl(${b} 70% 45%)"/></linearGradient></defs>`
    + `<rect width="64" height="64" fill="url(#g)"/><text x="32" y="42" text-anchor="middle" font-family="system-ui,-apple-system,sans-serif" font-size="28" font-weight="600" fill="#fff">${letter}</text></svg>`);
};
const avatar = (email) => {
  email = email?.trim().toLowerCase();
  if (email && !avatars.has(email)) avatars.set(email, gradient(email));
  return html`<img class="avatar" src=${email ? avatars.get(email) : nobody} alt="">`;
};

const known = (t) => { const d = new Date(t); return !isNaN(d) && d.getFullYear() > 2000 ? d : null; };
const hhmm = (t) => known(t)?.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }) ?? "";
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
const kb = (bytes) => Math.round((bytes ?? 0) / 1024);

// How the swarm keeps up: quiet when the download covers the stream, "tight" just under it, "short"
// only once it has been under for a while (bifrost's starving flag). A dip to 0.98x is not an alarm.
const swarmTone = (sw) => sw?.starving ? "short" : sw?.headroom > 0 && sw.headroom < 1 ? "tight" : "";

// Only the speeds take a colour, never the peers.
const swarm = (sw, speedsOnly = false) => {
  if (!sw?.sampled_at) return nothing;
  const down = kb(sw.download_bytes_per_second), rate = kb(sw.bitrate_bytes_per_second);
  const title = rate
    ? `The engine downloads ${down} KB/s of a ${rate} KB/s stream (${sw.headroom.toFixed(2)}x)${sw.starving ? ", short for a while: its buffer is draining" : ""}`
    : "Engine download; the bitrate is known after a few seconds of playing";
  return html` · ${speedsOnly ? nothing : `${plural(sw.peers, "peer")} · `}<span class=${swarmTone(sw)} title=${title}>${rate ? `${down}/${rate} KB/s` : `${down} KB/s`}</span>${sw.stalls ? ` · ${plural(sw.stalls, "stall")}` : ""}`;
};

function channelState(channel, jellyfin) {
  if (channel.playing) {
    // Who, and the swarm only when it is not keeping up: the rest is on Activity.
    const who = jellyfin.filter((p) => p.channel === channel.name).map((p) => p.user).filter(Boolean).join(", ");
    const sw = channel.playing.swarm;
    if (channel.playing.waiting) return { tone: "live", text: html`Waiting for the broadcast · ${who || plural(channel.playing.viewers, "viewer")}` };
    return { tone: "live", text: html`Playing · ${who || plural(channel.playing.viewers, "viewer")}${swarmTone(sw) ? swarm(sw, true) : nothing}` };
  }
  const o = channel.last_open;
  if (!o) return { tone: "none", text: "Not opened yet" };
  if (o.outcome === "playing") return { tone: "played", text: `Played at ${hhmm(o.at)}${o.device ? ` on ${o.device}` : ""} · first video in ${secs(o.seconds)}` };
  if (o.outcome === "gave_up") return { tone: "bad", text: `No source played at ${hhmm(o.at)} (${secs(o.seconds)})` };
  return { tone: "warn", text: `Left before it started at ${hhmm(o.at)} (${secs(o.seconds)})` };
}

const base = css`
  :host { display: block; }
  * { box-sizing: border-box; }
  .card { background: var(--surface); border: 1px solid var(--line); border-radius: 14px; padding: 18px; min-width: 0; }
  .muted { color: var(--muted); }
  .mono { font-family: var(--font-data); font-variant-numeric: tabular-nums; }
  .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; display: inline-block; }
  .dot.live { background: var(--ok); box-shadow: 0 0 0 3px color-mix(in srgb, var(--ok) 25%, transparent); }
  .dot.ok { background: var(--ok); } .dot.played { background: var(--played); }
  .dot.bad { background: var(--calm-bad); } .dot.warn { background: var(--calm-warn); }
  .dot.none { background: transparent; border: 1.5px solid var(--line); }
  .ok { color: var(--ok); } .bad { color: var(--calm-bad); } .warn { color: var(--calm-warn); } .live { color: var(--accent); }
  .tight { color: var(--calm-warn); } .short { color: var(--calm-bad); } .quick { color: var(--calm-ok); }
  h2 { font-size: 14px; color: var(--muted); margin: 26px 0 10px; font-weight: 600; }
  h2.first { margin-top: 0; }
  .src { font-family: var(--font-data); font-size: 12px; white-space: nowrap; }
  button.src { font: inherit; font-family: var(--font-data); font-size: 12px; color: var(--accent); background: none; border: 0; border-radius: 5px; padding: 0 4px; margin: 0 -4px; cursor: pointer; }
  button.src:hover, button.src:focus-visible { background: color-mix(in srgb, var(--accent) 12%, transparent); }
  button.src:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
`;

const switchable = (c) => c && c.ids.length > 1 && c.playing && !c.playing.waiting;
// A rainbow filled up to done of total, the half of the source being opened counted.
const progress = (done, total) => `--done: ${Math.min(100, Math.round((100 * (done + 0.5)) / total))}%`;
const toast = (from, text) => from.dispatchEvent(new CustomEvent("toast", { detail: text, bubbles: true, composed: true }));

class SourcePicker extends LitElement {
  static properties = { channel: { attribute: false }, reload: { attribute: false }, count: { type: Boolean }, pick: { state: true }, zoom: { state: true }, edit: { state: true }, check: { state: true }, opened: { state: true } };
  static styles = [base, css`
    :host { display: inline; }
    .link:focus-visible, .x:focus-visible, .ctl:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .link { font: inherit; color: var(--accent); background: none; border: 0; padding: 0; cursor: pointer; white-space: nowrap; }
    .link:hover, .link:focus-visible { text-decoration: underline; text-underline-offset: 2px; }
    .ctl { font: inherit; font-size: 12px; color: var(--accent); background: color-mix(in srgb, var(--accent) 10%, transparent); border: 0; border-radius: 6px; padding: 2px 8px; cursor: pointer; white-space: nowrap; }
    .ctl:hover { background: color-mix(in srgb, var(--accent) 18%, transparent); }
    .ctl:disabled { color: var(--muted); background: var(--chip); opacity: .7; cursor: default; }
    dialog { width: min(560px, calc(100vw - 32px)); border: 1px solid var(--line); border-radius: 14px; padding: 0; background: var(--surface); color: var(--ink); box-shadow: 0 20px 60px rgba(0, 0, 0, .35); }
    dialog::backdrop { background: rgba(0, 0, 0, .45); }
    .sheet { position: relative; padding: 18px; outline: 0; }
    dialog h3 { margin: 0 32px 2px 0; font-size: 14px; text-transform: none; letter-spacing: 0; color: var(--ink); display: block; }
    dialog p { margin: 0 0 12px; font-size: 12px; color: var(--muted); }
    .panel { list-style: none; margin: 0; padding: 0; border: 1px solid var(--line); border-radius: 10px; overflow: hidden; }
    .panel li { border-top: 1px solid var(--line); }
    .panel li:first-child { border-top: 0; }
    .panel li.line { display: grid; grid-template-columns: 96px minmax(0, 1fr) auto; gap: 3px 12px; align-items: center; padding: 9px 12px; font-size: 13px; }
    .line .thumb { grid-row: 1 / 3; width: 96px; aspect-ratio: 16 / 9; border-radius: 6px; overflow: hidden; background: var(--chip); border: 0; padding: 0; display: grid; place-items: center; font-size: 10px; color: var(--muted); }
    .line button.thumb { cursor: zoom-in; }
    .line .thumb img { width: 100%; height: 100%; object-fit: cover; display: block; }
    .line .info { grid-column: 2 / 4; font-size: 12px; color: var(--muted); font-variant-numeric: tabular-nums; }
    .info > * + * { margin-left: 10px; }
    .id { font-family: var(--font-data); font-size: 11px; color: var(--muted); margin-left: 8px; user-select: all; }
    .acts { display: flex; gap: 6px; }
    .ctl.minus { color: var(--calm-bad); background: color-mix(in srgb, var(--calm-bad) 10%, transparent); }
    .ctl.minus:hover { background: color-mix(in srgb, var(--calm-bad) 18%, transparent); }
    .head { display: flex; justify-content: space-between; align-items: baseline; gap: 12px; margin: 0 0 12px; }
    .head p { margin: 0; }
    .gap { margin-left: 10px; }
    .checking { display: inline-flex; align-items: center; gap: 8px; font-size: 12px; color: var(--muted); white-space: nowrap; font-variant-numeric: tabular-nums; }
    .bar { width: 72px; height: 4px; border-radius: 4px; background: var(--chip); position: relative; }
    .bar::before { content: ""; position: absolute; inset: 0; border-radius: 4px; background: var(--spectrum);
      clip-path: inset(0 calc(100% - var(--done)) 0 0); transition: clip-path .6s ease-out; }
    @media (prefers-reduced-motion: reduce) { .bar::before { transition: none; } }
    .removed { margin-top: 12px; font-size: 12px; color: var(--muted); }
    .removed summary { cursor: pointer; margin-bottom: 8px; }
    .zoom { position: absolute; inset: 0; background: var(--surface); border-radius: 14px; padding: 18px; display: flex; flex-direction: column; gap: 8px; cursor: zoom-out; }
    .zoom img { width: 100%; border-radius: 8px; display: block; }
    .zoom p { margin: 0; }
    .x { position: absolute; top: 12px; right: 12px; font: inherit; font-size: 14px; line-height: 1; color: var(--muted); background: var(--chip); border: 0; border-radius: 6px; padding: 5px 8px; cursor: pointer; }
    .tag { font-family: var(--font-data); font-size: 11px; border-radius: 5px; padding: 0 6px; margin-left: 6px; color: var(--muted); background: var(--chip); }
    .tag.on { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent); }
  `];

  // The sheet renders only once opened: the channel list holds one picker per channel.
  async show() {
    this.opened = true;
    await this.updateComplete;
    this.renderRoot.querySelector("dialog").showModal();
  }

  async choose(x) {
    const c = this.channel;
    this.pick = { id: x.id };
    try {
      const r = await fetch(`api/channels/${c.slug}/source`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id: x.id }) })
        .catch(() => { throw new Error("Could not reach bifrost"); });
      if (!r.ok) {
        const body = await r.json().catch(() => null);
        throw new Error(body ? body.error || `Source ${x.n} did not start` : `No answer from bifrost (HTTP ${r.status})`);
      }
      await this.reload?.();
      this.pick = { done: x.n };
      this.renderRoot.querySelector("dialog")?.close();
      setTimeout(() => { this.pick = null; }, 4000);
    } catch (e) {
      this.pick = { id: x.id, error: e.message };
      setTimeout(() => { this.pick = null; }, 6000);
    }
  }

  async block(x, removed) {
    const c = this.channel;
    this.edit = { id: x.id };
    try {
      const r = await fetch(`api/channels/${c.slug}/blocked/${x.id}`, { method: removed ? "POST" : "DELETE" })
        .catch(() => { throw new Error("Could not reach bifrost"); });
      if (!r.ok) {
        const body = await r.json().catch(() => null);
        throw new Error(body?.error || `No answer from bifrost (HTTP ${r.status})`);
      }
      await this.reload?.();
      this.edit = null;
    } catch (e) {
      this.edit = { id: x.id, error: e.message };
      setTimeout(() => { this.edit = null; }, 6000);
    }
  }

  // Polls faster than the page while bifrost opens the other sources one by one.
  async survey() {
    const c = this.channel;
    this.check = { busy: true };
    try {
      const r = await fetch(`api/channels/${c.slug}/survey`, { method: "POST" }).catch(() => { throw new Error("Could not reach bifrost"); });
      const body = await r.json().catch(() => null);
      if (!r.ok) throw new Error(body?.error || `No answer from bifrost (HTTP ${r.status})`);
      if (!body.checking) toast(this, "Every source of this channel was checked a moment ago");
      while (body.checking) {
        await new Promise((ok) => setTimeout(ok, 2000));
        await this.reload?.();
        if (!this.channel.survey) break;
      }
    } catch (e) {
      toast(this, e.message);
    }
    this.check = null;
  }

  surveyNote() {
    const sv = this.channel.survey;
    if (sv) return html`<span class="checking" role="status"><span class="bar" style=${progress(sv.done, sv.total)}></span>${sv.done + 1} of ${sv.total}</span>`;
    return html`<button class="link" ?disabled=${this.check?.busy} @click=${() => this.survey()}>Check the others</button>`;
  }

  thumb(x) {
    if (!known(x.thumb_at)) return html`<span class="thumb">no preview</span>`;
    const src = `api/thumbs/${x.id}?t=${Date.parse(x.thumb_at)}`;
    return html`<button class="thumb" aria-label="Enlarge the picture of ${x.id.slice(0, 8)}" @click=${() => { this.zoom = { ...x, src }; }}><img src=${src} alt="" loading="lazy"></button>`;
  }

  // Numbered in the playlist's order, so a source keeps its number whichever plays.
  ranked() {
    const c = this.channel;
    return (c.sources?.length ? c.sources : c.ids.map((id) => ({ id }))).map((x, i) => ({ ...x, n: i + 1 }))
      .sort((a, b) => (a.outcome === "failed") - (b.outcome === "failed") || (b.headroom ?? -1) - (a.headroom ?? -1));
  }

  facts(x) {
    const kept = x.kept ? html`<span class="muted">Not in the list</span>` : "";
    if (!x.seen_at && x.headroom == null) return [kept, html`<span class="muted">Not tried yet</span>`].filter(Boolean);
    const tone = x.headroom == null ? "" : swarmTone({ headroom: x.headroom }) || "quick";
    const bits = [kept, x.outcome === "failed" ? html`<span class="bad">Failed</span>` : "", x.headroom != null ? html`<span class=${tone}>${x.headroom.toFixed(2)}x</span>` : "",
      x.height ? `${x.height}p` : "", x.audio, x.peers != null ? plural(x.peers, "peer") : "", hhmm(x.seen_at)].filter(Boolean);
    return bits.map((b) => html`<span>${b}</span>`);
  }

  quick() {
    const pick = this.pick, next = this.ranked().find((x) => x.id !== this.channel.playing.source && x.outcome !== "failed");
    if (pick?.id && !pick.error) return html`<span class="muted">switching…</span>`;
    if (pick?.error) return html`<span class="bad">${pick.error}</span>`;
    if (pick?.done) return html`<span class="muted">Switched to Source ${pick.done}</span>`;
    return next ? html`<button class="link" @click=${() => this.choose(next)}>Switch to best</button>` : nothing;
  }

  render() {
    const c = this.channel, live = switchable(c);
    const quick = live ? this.quick() : nothing;
    return html`${this.count ? html`<button class="src" aria-haspopup="dialog" @click=${() => this.show()}>${plural(c.ids.length, "source")}</button>` : nothing}${quick === nothing ? nothing : html`<span class="gap">${quick}</span>`}
    ${this.opened ? this.sheet(c, live) : nothing}`;
  }

  sheet(c, live) {
    const pick = this.pick, pending = pick?.id && !pick.error;
    return html`<dialog aria-label="Sources of ${c.name}" @click=${(e) => { if (e.target === e.currentTarget) e.target.close(); }}>
      <div class="sheet" tabindex="-1" autofocus><button class="x" aria-label="Close" @click=${(e) => e.target.closest("dialog").close()}>✕</button><h3>${c.name}</h3><div class="head"><p>${live ? "Pick a source for this channel. Everyone watching it switches too." : "Not playing now: Play shows up here while it plays."}</p>${this.surveyNote()}</div>
        <ul class="panel">${this.ranked().map((x, i) => {
          const on = x.id === c.playing?.source, mine = pick?.id === x.id, edit = this.edit?.id === x.id ? this.edit : null;
          return html`<li class="line">
            ${this.thumb(x)}
            <span><b>Source ${x.n}</b><span class="id" title=${x.id}>${x.id.slice(0, 8)}</span>${i === 0 && x.headroom != null && x.outcome !== "failed" ? html`<span class="tag">best</span>` : nothing}${c.survey?.current === x.id ? html`<span class="tag on">checking</span>` : nothing}${mine && pick.error ? html` <span class="bad">${pick.error}</span>` : nothing}${edit?.error ? html` <span class="bad">${edit.error}</span>` : nothing}</span>
            <span class="acts">${on ? html`<span class="tag on">Playing</span>`
              : html`${live ? html`<button class="ctl" ?disabled=${pending} @click=${() => this.choose(x)}>${mine && pending ? "switching…" : "Play"}</button>` : nothing}
                <button class="ctl minus" title="Remove from this channel" aria-label="Remove source ${x.n} from this channel" ?disabled=${pending || edit || c.ids.length < 2} @click=${() => this.block(x, true)}>−</button>`}</span>
            <span class="info">${this.facts(x)}</span></li>`;
        })}</ul>
        ${c.blocked?.length ? html`<details class="removed"><summary>${plural(c.blocked.length, "removed source")}</summary>
          <ul class="panel">${c.blocked.map((x) => {
            const edit = this.edit?.id === x.id ? this.edit : null;
            return html`<li class="line">${this.thumb(x)}
              <span><span class="id" title=${x.id}>${x.id.slice(0, 8)}</span>${edit?.error ? html` <span class="bad">${edit.error}</span>` : nothing}</span>
              <span class="acts"><button class="ctl" ?disabled=${edit} @click=${() => this.block(x, false)}>Restore</button></span>
              <span class="info">${this.facts(x)}</span></li>`;
          })}</ul></details>` : nothing}
        ${this.zoom ? html`<div class="zoom" @click=${() => { this.zoom = null; }}><img src=${this.zoom.src} alt="Picture of ${this.zoom.id.slice(0, 8)}"><p class="muted"><span class="id" title=${this.zoom.id}>${this.zoom.id}</span> ${hhmm(this.zoom.thumb_at)}</p></div>` : nothing}</div>
    </dialog>`;
  }
}
customElements.define("source-picker", SourcePicker);

class ChannelGroup extends LitElement {
  static styles = [base, css`
    .card { height: 300px; display: flex; flex-direction: column; }
    h3 { margin: 0 0 6px; font-size: 12px; letter-spacing: .08em; text-transform: uppercase; color: var(--muted); display: flex; justify-content: space-between; gap: 8px; }
    h3 span { font-family: var(--font-data); letter-spacing: 0; text-transform: none; font-weight: 500; }
    ul { list-style: none; margin: 0 0 0 -4px; padding: 0 4px; overflow-y: auto; min-height: 0; flex: 1; scrollbar-width: thin; }
    li { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; gap: 2px 10px; align-items: center; padding: 8px 0; border-top: 1px solid var(--line); }
    li:first-child { border-top: 0; }
    .name { font-weight: 600; font-size: 14px; min-width: 0; }
    .state { grid-column: 2 / 4; font-size: 12px; color: var(--muted); }
    .state.played, .state.warn, .state.live { color: var(--muted); }
    .air { grid-column: 2 / 4; font-size: 12px; color: var(--ink); min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .src { color: var(--muted); }
  `];

  static properties = { name: {}, channels: { attribute: false }, jellyfin: { attribute: false }, reload: { attribute: false } };

  count(c) {
    return html`<button class="src" aria-haspopup="dialog" @click=${(e) => e.target.closest("li").querySelector("source-picker").show()}>${plural(c.ids.length, "source")}</button>`;
  }

  render() {
    return html`<section class="card">
      <h3>${this.name}<span>${this.channels.length}</span></h3>
      <ul>${this.channels.map((c) => {
        const st = channelState(c, this.jellyfin);
        return html`<li>
          <i class="dot ${st.tone}" aria-hidden="true"></i>
          <span class="name">${c.name}</span>
          ${this.count(c)}
          ${c.now ? html`<span class="air" title="${c.now.title}, until ${hhmm(c.now.stop)}">${c.now.title} · until ${hhmm(c.now.stop)}</span>` : nothing}
          <span class="state ${st.tone === "none" ? "" : st.tone}">${st.text}<source-picker .channel=${c} .reload=${this.reload}></source-picker></span>
        </li>`;
      })}</ul>
    </section>`;
  }
}
customElements.define("channel-group", ChannelGroup);

class ChannelBoard extends LitElement {
  static properties = { channels: { attribute: false }, jellyfin: { attribute: false }, reload: { attribute: false }, query: { state: true }, first: { state: true } };
  static styles = [base, css`
    .tools { display: flex; flex-wrap: wrap; gap: 8px 14px; align-items: center; margin-bottom: 14px; }
    input { font: inherit; font-size: 14px; border: 1px solid var(--line); background: var(--surface); color: var(--ink); border-radius: 10px; padding: 8px 12px; width: min(100%, 300px); }
    input:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .legend { display: flex; flex-wrap: wrap; gap: 6px 14px; font-size: 12px; color: var(--muted); }
    .legend button { font: inherit; color: inherit; background: none; border: 0; border-radius: 6px; padding: 2px 6px; margin: -2px -6px; display: inline-flex; align-items: center; gap: 6px; cursor: pointer; }
    .legend button:hover, .legend button:focus-visible { color: var(--ink); }
    .legend button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .legend button[aria-pressed="true"] { color: var(--ink); background: var(--chip); }
    .groups { display: grid; gap: 14px; grid-template-columns: repeat(auto-fill, minmax(min(100%, 320px), 1fr)); }
    .empty { color: var(--muted); font-size: 14px; margin: 0; }
  `];

  constructor() { super(); this.channels = []; this.jellyfin = []; this.query = ""; this.first = null; }

  groups() {
    const needle = this.query.trim().toLowerCase();
    const out = [], at = new Map();
    for (const c of this.channels) {
      const group = c.group || "Other";
      if (needle && !c.name.toLowerCase().includes(needle) && !group.toLowerCase().includes(needle)) continue;
      if (!at.has(group)) { at.set(group, out.length); out.push({ name: group, channels: [] }); }
      out[at.get(group)].channels.push(c);
    }
    if (!this.first) return out;
    // Channels in the picked state first, and the groups holding any of them first; the rest keep their order.
    const picked = (c) => channelState(c, this.jellyfin).tone === this.first;
    for (const g of out) g.channels.sort((a, b) => picked(b) - picked(a));
    return out.sort((a, b) => picked(b.channels[0]) - picked(a.channels[0]));
  }

  render() {
    const groups = this.groups();
    return html`
      <div class="tools">
        <input type="search" placeholder="Search channels…" aria-label="Search channels" .value=${this.query} @input=${(e) => { this.query = e.target.value; }}>
        <div class="legend" role="group" aria-label="Show first">
          ${[["live", "Playing"], ["played", "Last open played"], ["warn", "Last open left early"], ["bad", "Last open failed"], ["none", "Not opened yet"]].map(([tone, label]) =>
            html`<button aria-pressed=${this.first === tone} @click=${() => { this.first = this.first === tone ? null : tone; }}><i class="dot ${tone}"></i>${label}</button>`)}
        </div>
      </div>
      ${groups.length
        ? html`<div class="groups">${groups.map((g) => html`<channel-group .name=${g.name} .channels=${g.channels} .jellyfin=${this.jellyfin} .reload=${this.reload}></channel-group>`)}</div>`
        : html`<p class="empty">${this.channels.length ? "No channel matches." : "No channel list yet."}</p>`}`;
  }
}
customElements.define("channel-board", ChannelBoard);

class ActivityView extends LitElement {
  static properties = { status: { attribute: false }, channels: { attribute: false }, reload: { attribute: false } };
  static styles = [base, css`
    .row { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 14px; align-items: center; }
    .row > div { min-width: 0; overflow-wrap: anywhere; }
    .row + .row { margin-top: 12px; }
    .devices { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; }
    .devices li { display: grid; grid-template-columns: none; padding: 0; border: 0; }
    .ring { width: 44px; height: 44px; border-radius: 50%; border: 2px dashed var(--line); display: grid; place-items: center; color: var(--muted); }
    .ring.on { border: 2px solid var(--accent); color: var(--accent); }
    .row p { margin: 2px 0 0; color: var(--muted); font-size: 14px; max-width: 62ch; }
    .row p.air { color: var(--ink); font-size: 15px; font-weight: 500; }
    .row p.air span { color: var(--muted); font-weight: 400; }
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
    .dev { grid-column: 2 / 4; font-size: 12px; color: var(--muted); }
    .source { font: inherit; color: inherit; background: none; border: 0; padding: 0; cursor: pointer; text-decoration: underline dotted; text-underline-offset: 3px; }
    .source:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .res { font-family: var(--font-data); font-size: 12px; white-space: nowrap; }
    .tag { font-family: var(--font-data); font-size: 11px; border-radius: 5px; padding: 0 6px; margin-left: 6px; vertical-align: 1px; display: inline-block; white-space: nowrap; overflow-wrap: normal; }
    .tag.copy { background: color-mix(in srgb, var(--ok) 16%, transparent); color: var(--ok); }
    .tag.trans { background: color-mix(in srgb, var(--warn) 18%, transparent); color: var(--warn); }
    .tag.slow { background: color-mix(in srgb, var(--calm-bad) 16%, transparent); color: var(--calm-bad); }
    /* Capped at two channels and five opens (set in updated()); not on a phone, where a scroll inside a scroll traps the thumb. */
    .scroller { max-height: var(--fit, none); overflow-y: auto; scrollbar-width: thin; margin-right: -8px; padding-right: 8px; }
    @media (max-width: 600px) { .scroller { max-height: none; overflow: visible; margin-right: 0; padding-right: 0; } }
    .stream > p.muted { font-size: 12px; margin: 10px 0 0; }
    .stream + .stream { margin-top: 18px; padding-top: 18px; border-top: 1px solid var(--line); }
    .facts { display: grid; gap: 14px 24px; grid-template-columns: repeat(auto-fit, minmax(min(100%, 190px), 1fr)); margin-top: 14px; }
    .fact { min-width: 0; font-size: 14px; }
    .fact .lbl { display: block; font-size: 12px; color: var(--muted); margin-bottom: 2px; }
    .fact .sub { display: block; font-size: 12px; color: var(--muted); overflow-wrap: anywhere; }
    .gauge { position: relative; height: 6px; border-radius: 6px; background: var(--chip); margin: 7px 0 4px; max-width: 220px; }
    .gauge i { position: absolute; inset: 0 auto 0 0; border-radius: 6px; background: var(--muted); }
    .gauge.tight i { background: var(--calm-warn); } .gauge.short i { background: var(--calm-bad); }
    .gauge::after { content: ""; position: absolute; top: -3px; bottom: -3px; left: calc(100% / 1.5); width: 1.5px; background: var(--ink); opacity: .5; }
    .tag.probe { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent); }
  `];

  screen(on) {
    return html`<div class="ring ${on ? "on" : ""}" aria-hidden="true"><svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><rect x="3" y="5" width="18" height="12" rx="2"/><path d="M8 21h8"/></svg></div>`;
  }

  now() {
    const streams = this.status.streams || [], jellyfin = this.status.jellyfin_playing || [];
    if (!streams.length) {
      return html`<div class="row">${this.screen(false)}<div><b>Nobody is watching</b>
        <p>When a channel is open it shows here: what it airs by the guide, its source, how many are watching, how soon the engine gave the first video, its peers and whether it downloads faster than the channel plays, whether Jellyfin copies the video or re-encodes it, and whether it keeps up with live.</p></div></div>`;
    }
    return html`<div class="scroller">${streams.map((st) => this.stream(st, jellyfin))}</div>`;
  }

  stream(st, jellyfin) {
    const watchers = jellyfin.filter((p) => st.channel.startsWith(p.channel));
    const jf = watchers[0];
    const transcoding = jf && jf.video !== "copy";
    return html`<div class="stream"><div class="row">${this.screen(true)}<div>
      <b>${st.channel}</b>${jf ? (transcoding
        ? html`<span class="tag trans" title="Jellyfin re-encodes the video on the Pi">re-encoding</span>`
        : html`<span class="tag copy" title="Jellyfin passes the video through untouched">direct</span>`) : nothing}${transcoding && jf.speed && jf.speed < 0.95 ? html`<span class="tag slow" title="Jellyfin's re-encode against live: below 1x the picture stalls">falling behind · ${jf.speed.toFixed(2)}x</span>` : nothing}${st.probing ? html`<span class="tag probe" title="Measuring another source of the channel next to this one, to move to it if it is clearly better">trying ${st.probing_source.slice(0, 8)}…</span>` : nothing}
      ${st.now ? html`<p class="air">${st.now.title} <span>until ${hhmm(st.now.stop)}${st.next ? `, then ${st.next.title}` : ""}</span></p>` : nothing}
    </div></div>
    <div class="facts">
      ${this.watching(st, watchers)}
      ${this.swarmFact(st.swarm)}
      <div class="fact"><span class="lbl">Source</span>${this.source(st.source)}
        <span class="sub">since ${hhmm(st.since)}, first video in ${secs(st.started_after_s)}</span>
        ${switchable(this.channels?.find((c) => c.slug === st.slug)) ? html`<span class="sub"><source-picker count .channel=${this.channels.find((c) => c.slug === st.slug)} .reload=${this.reload}></source-picker></span>` : nothing}
        ${transcoding && jf.reasons ? html`<span class="sub">Jellyfin re-encodes: ${jf.reasons.join(", ")}</span>` : nothing}</div>
    </div>
    </div>`;
  }

  // A Jellyfin connection no session plays is one Jellyfin left open (a zap, a TV switched off), not a viewer.
  watching(st, watchers) {
    const external = st.external ?? 0, leftOpen = Math.max(0, st.viewers - external - watchers.length);
    const notes = [external > 0 ? html`<li class="muted">${plural(external, "viewer")} not through Jellyfin</li>` : "",
      leftOpen > 0 ? html`<li class="muted">Left open by Jellyfin, closes by itself</li>` : ""].filter(Boolean);
    if (!watchers.length && !external) return html`<div class="fact"><span class="lbl">Watching</span>Nobody${leftOpen ? html`<span class="sub">Left open by Jellyfin, closes by itself</span>` : nothing}</div>`;
    // One line per device: the same user on two screens is two lines, not one name.
    return html`<div class="fact"><span class="lbl">Watching</span><ul class="devices">${watchers.map((w) => html`<li>${w.user || w.device}<span class="sub">${[w.device, w.client].filter(Boolean).join(" · ")}</span></li>`)}${notes}</ul></div>`;
  }

  // The headroom bar is the loud thing on the card: the tick is what the stream needs (1x), the bar fills to 1.5x.
  swarmFact(sw) {
    if (!sw?.sampled_at) return html`<div class="fact"><span class="lbl">Swarm</span><span class="muted">measuring…</span></div>`;
    const down = kb(sw.download_bytes_per_second), rate = kb(sw.bitrate_bytes_per_second), tone = swarmTone(sw);
    const fill = rate ? Math.min(sw.headroom / 1.5, 1) * 100 : 0;
    return html`<div class="fact"><span class="lbl">Swarm</span>${plural(sw.peers, "peer")}
      ${rate ? html`<div class="gauge ${tone}" role="img" aria-label="${sw.headroom.toFixed(2)} times what the stream needs"><i style="width:${fill}%"></i></div>
        <span class="sub ${tone}">${down} KB/s of ${rate} needed · ${sw.headroom.toFixed(2)}x${tone === "short" ? ", buffer draining" : ""}</span>`
        : html`<span class="sub">${down} KB/s, bitrate known in a few seconds</span>`}
      ${sw.stalls ? html`<span class="sub">${plural(sw.stalls, "stall")}</span>` : nothing}</div>`;
  }

  // The short id copies a link to that one source, for a player that is not Jellyfin, like VLC.
  source(id) {
    if (!id) return "";
    const copy = async (e) => {
      await navigator.clipboard.writeText(`${location.origin}/ace/getstream?id=${id}`);
      e.target.textContent = "link copied";
      setTimeout(() => { e.target.textContent = `${id.slice(0, 8)}…`; }, 1500);
    };
    return html`<button class="source mono" title="${id} · click to copy a link VLC can play" @click=${copy}>${id.slice(0, 8)}…</button>`;
  }

  recent() {
    const recent = this.status.recent || [];
    if (!recent.length) return html`<li><span></span><span class="muted">No opens since bifrost started.</span><span></span></li>`;
    return recent.map((r) => {
      // Grey is normal; colour only says something: under a second, slow, or nothing played.
      const tone = r.outcome === "gave_up" ? "bad" : r.outcome !== "playing" ? "" : r.seconds < 1 ? "quick" : r.seconds > 5 ? "tight" : "";
      const res = r.outcome === "playing" ? secs(r.seconds) : r.outcome === "gave_up" ? "no video" : "left early";
      const title = r.outcome === "playing" ? `first video after ${secs(r.seconds)}` : `gave up after ${secs(r.seconds)}`;
      return html`<li><span class="at">${hhmm(r.at)}</span><span class="ch">${r.channel}</span><span class="res ${tone || "muted"}" title="${title}">${res}</span>
        ${r.device ? html`<span class="dev">${r.device}</span>` : nothing}</li>`;
    });
  }

  updated() {
    const fit = (box, n) => {
      const next = box?.children[n];
      box?.style.setProperty("--fit", next ? `${next.offsetTop - box.offsetTop}px` : "none");
    };
    fit(this.renderRoot.querySelector("div.scroller"), 2);
    fit(this.renderRoot.querySelector("ul.scroller"), 5);
  }

  render() {
    if (!this.status) return nothing;
    const l = this.status.list || {}, g = this.status.guide || {}, j = this.status.jellyfin || {};
    return html`
      <h2 class="first">Now</h2>
      <section class="card">${this.now()}</section>
      <div class="duo">
        <div><h2>Recent opens</h2><section class="card"><ul class="scroller">${this.recent()}</ul></section></div>
        <div><h2>List and guide</h2><section class="card">
          <div class="big"><div><b>${l.channels ?? "–"}</b><span>channels</span></div><div><b>${l.sources ?? "–"}</b><span>sources</span></div><div><b>${g.channels ?? "–"}</b><span>with guide</span></div></div>
          <dl>
            <dt>List</dt><dd>read ${ago(l.last_read)}${known(l.last_change) ? `; changed ${ago(l.last_change)}` : ""}${l.error ? html` · <span class="bad">${l.error}</span>` : nothing}</dd>
            <dt>Guide</dt><dd>${g.bytes ? `${Math.round(g.bytes / 1024)} KB for ${g.channels} channels` : "not cut yet"}</dd>
            <dt>Jellyfin</dt>${j.last_code
              ? j.last_code === 204
                ? html`<dd class="ok">reread the guide ${ago(j.last_refresh)}</dd>`
                : html`<dd class="bad">guide refresh failed ${ago(j.last_refresh)} (HTTP ${j.last_code})</dd>`
              : html`<dd>no refresh asked yet</dd>`}
            ${j.refresh_due ? html`<dt>Next</dt><dd>refresh due at ${hhmm(j.refresh_due)}</dd>` : nothing}
          </dl>
        </section></div>
      </div>`;
  }
}
customElements.define("activity-view", ActivityView);

class ShareDialog extends LitElement {
  static properties = { access: { attribute: false }, heading: {}, data: { state: true }, error: { state: true }, busy: { state: true }, copied: { state: true }, created: { state: true } };
  static styles = [base, css`
    dialog { width: min(520px, calc(100vw - 32px)); border: 1px solid var(--line); border-radius: 14px; padding: 22px;
      background: var(--surface); color: var(--ink); box-shadow: 0 20px 60px rgba(0, 0, 0, .35); }
    dialog::backdrop { background: rgba(0, 0, 0, .45); }
    h3 { margin: 0 32px 8px 0; font-family: var(--font-display); font-weight: 500; font-size: 20px; }
    .close { position: absolute; top: 14px; right: 14px; }
    p.lead { margin: 0 0 16px; color: var(--muted); font-size: 14px; }
    form, .copy { display: flex; gap: 8px; }
    input { flex: 1; min-width: 0; font: inherit; font-size: 14px; color: var(--ink); background: var(--bg); border: 1px solid var(--line); border-radius: 8px; padding: 9px 11px; }
    input:focus-visible, button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .btn { font: inherit; font-size: 14px; font-weight: 600; border-radius: 8px; padding: 9px 14px; cursor: pointer; border: 1px solid var(--line); background: var(--chip); color: var(--ink); white-space: nowrap; }
    .btn.primary { background: var(--accent); border-color: var(--accent); color: var(--bg); }
    .btn:disabled { opacity: .5; cursor: default; }
    .hint { font-size: 12px; color: var(--muted); margin: 6px 0 0; }
    .err { font-size: 13px; color: var(--calm-bad); margin: 8px 0 0; }
    h4 { font-size: 13px; font-weight: 600; color: var(--muted); margin: 20px 0 8px; }
    ul { list-style: none; margin: 0; padding: 0; border: 1px solid var(--line); border-radius: 10px; }
    li { display: flex; align-items: center; gap: 10px; padding: 9px 12px; border-top: 1px solid var(--line); font-size: 14px; }
    li:first-child { border-top: 0; }
    .avatar { width: 30px; height: 30px; border-radius: 50%; flex: none; background: var(--chip); }
    .who { flex: 1; min-width: 0; overflow-wrap: anywhere; }
    .who small { display: block; color: var(--muted); font-size: 12px; }
    .remove { font-size: 12px; padding: 5px 10px; }
    .role { font: inherit; font-size: 12px; font-weight: 600; border-radius: 8px; padding: 5px 8px; border: 1px solid var(--line); background: var(--chip); color: var(--ink); cursor: pointer; }
    .role:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .fixed { font-size: 12px; color: var(--muted); }
  `];

  constructor() { super(); this.data = null; this.error = ""; this.busy = false; this.copied = false; this.created = ""; }

  async open() {
    this.error = ""; this.data = null; this.created = "";
    this.renderRoot.querySelector("dialog").showModal();
    try {
      const r = await fetch("api/access?lists=1");
      const data = await r.json();
      if (!r.ok) throw new Error(data.error || `HTTP ${r.status}`);
      this.data = data;
    } catch (e) {
      this.error = e.message;
    }
  }

  async change(method, body, path = "api/access/members") {
    this.busy = true; this.error = "";
    try {
      const r = await fetch(path, { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
      const data = await r.json();
      if (!r.ok) throw new Error(data.error || `HTTP ${r.status}`);
      this.data = data;
      return true;
    } catch (e) {
      this.error = e.message;
      return false;
    } finally {
      this.busy = false;
    }
  }

  async invite(e, group) {
    e.preventDefault();
    const input = e.target.querySelector("input");
    const emails = input.value.split(",").map((x) => x.trim()).filter(Boolean);
    if (emails.length && await this.change("POST", { group: group.id, emails })) input.value = "";
  }

  async createLink(group) {
    if (await this.change("POST", { group: group.id }, "api/access/links")) this.created = this.data.created;
  }

  copy(text) {
    navigator.clipboard.writeText(text);
    this.copied = true;
    setTimeout(() => { this.copied = false; }, 1500);
  }

  invited(email) {
    const i = this.data.invites?.[email];
    if (!i) return nothing;
    const by = i.by === this.access.me?.toLowerCase() ? "you" : i.by;
    return html`<small>invited by ${by} · ${new Date(i.at).toLocaleDateString([], { day: "numeric", month: "short" })}</small>`;
  }

  members(group) {
    if (!group.members.length) return html`<li class="muted">Nobody yet.</li>`;
    const me = this.access.me?.toLowerCase(), owners = this.data.owners ?? [], admins = this.data.admins ?? [];
    const isAdmin = (m) => owners.includes(m) || admins.includes(m);
    return group.members.map((m) => html`<li>${avatar(m)}
      <span class="who">${m}${m === me ? html`<small>you · admin</small>` : this.invited(m)}</span>
      ${group.editable && m !== me ? html`${owners.includes(m)
        ? html`<span class="fixed" title="Set in bifrost's settings">Admin</span>`
        : html`<select class="role" aria-label="Role of ${m}" ?disabled=${this.busy} @change=${(e) => this.change("PATCH", { email: m, role: e.target.value })}>
            <option value="viewer" ?selected=${!isAdmin(m)}>Viewer</option><option value="admin" ?selected=${isAdmin(m)}>Admin</option></select>`}
        <button class="btn remove" ?disabled=${this.busy} @click=${() => this.change("DELETE", { group: group.id, email: m })}>Remove</button>` : nothing}</li>`);
  }

  copyBox(text, label) {
    return html`<div class="copy"><input readonly .value=${text} aria-label=${label} @focus=${(e) => e.target.select()}>
      <button class="btn primary" @click=${() => this.copy(text)}>${this.copied ? "Copied" : "Copy"}</button></div>`;
  }

  joinSection(group) {
    const links = Object.entries(this.data.links ?? {}).sort(([, a], [, b]) => a.expires.localeCompare(b.expires));
    const day = (t) => new Date(t).toLocaleDateString([], { day: "numeric", month: "short" });
    return html`${this.created
        ? html`${this.copyBox(this.created, "Invite link")}<p class="hint">Send it by any chat. It lets one person in, for a week: they sign in with Google or a code sent to their email and become a member.</p>`
        : html`<button class="btn primary" ?disabled=${this.busy} @click=${() => this.createLink(group)}>Create invite link</button>
          <p class="hint">Whoever opens it first gets in, so send it only to the person you mean.</p>`}
      ${links.length ? html`<h4>Invite links not used yet</h4><ul>${links.map(([id, l]) => html`<li>
        <span class="who">Made by ${l.by === this.access.me?.toLowerCase() ? "you" : l.by}<small>works until ${day(l.expires)}</small></span>
        <button class="btn remove" ?disabled=${this.busy} @click=${() => this.change("DELETE", { id }, "api/access/links")}>Revoke</button></li>`)}</ul>` : nothing}`;
  }

  render() {
    if (!this.access?.admin) return nothing;
    const a = this.data, editable = a?.groups.find((g) => g.editable);
    return html`<dialog aria-labelledby="share-title">
      <button class="btn close" aria-label="Close" @click=${() => this.renderRoot.querySelector("dialog").close()}>✕</button>
      <h3 id="share-title">Share ${this.heading}</h3>
      <p class="lead">People on this list can open this page after signing in with Google or a code sent to their email. They see the page only: streams never leave the house.</p>
      ${!a ? html`${this.error ? nothing : html`<p class="hint">Loading who has access…</p>`}` : html`
      ${editable
        ? html`${a.joinable ? this.joinSection(editable) : nothing}
          <h4>${a.joinable ? "Or add their email" : "Add their email"}</h4>
          <form @submit=${(e) => this.invite(e, editable)}>
            <input type="text" inputmode="email" autocomplete="off" placeholder="user@example.com" aria-label="Email addresses">
            <button class="btn ${a.joinable ? "" : "primary"}" ?disabled=${this.busy}>Add</button></form>
          <p class="hint">They must sign in with this exact address. Separate several with commas.</p>`
        : html`<p class="hint">No list here can be edited from bifrost: each one may also let people into something else.</p>`}
      ${a.groups.map((g) => html`<h4>${g.editable ? "Members" : `${g.name} · managed elsewhere`}</h4><ul>${this.members(g)}</ul>`)}
      ${editable ? html`<p class="hint">Removing someone stops new sign-ins at once; a page they already have open keeps working until their session ends (up to a day).</p>` : nothing}`}
      ${this.error ? html`<p class="err" role="alert">${this.error}</p>` : nothing}
    </dialog>`;
  }
}
customElements.define("share-dialog", ShareDialog);

class BifrostApp extends LitElement {
  static properties = { status: { state: true }, channels: { state: true }, view: { state: true }, reachable: { state: true }, access: { state: true }, note: { state: true } };
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
    .me { background: none; border: 0; padding: 0; border-radius: 50%; cursor: pointer; display: flex; }
    .me:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .avatar { width: 32px; height: 32px; border-radius: 50%; background: var(--chip); }
    .menu { inset: auto; margin: 0; border: 1px solid var(--line); border-radius: 12px; background: var(--surface); color: var(--ink);
      padding: 14px; min-width: 220px; max-width: min(300px, calc(100vw - 32px)); box-shadow: 0 12px 40px rgba(0, 0, 0, .3); }
    .menu .who { display: flex; align-items: center; gap: 10px; font-size: 13px; overflow-wrap: anywhere; margin-bottom: 12px; }
    .menu .avatar { width: 40px; height: 40px; }
    .menu .item { display: block; width: 100%; box-sizing: border-box; text-align: center; font: inherit; font-size: 13px; font-weight: 600; color: var(--ink);
      text-decoration: none; border: 1px solid var(--line); border-radius: 8px; padding: 7px 12px; background: var(--chip); cursor: pointer; }
    .menu .item + .item { margin-top: 8px; }
    .toast { position: fixed; inset: auto; margin: 0; left: max(16px, env(safe-area-inset-left, 0px)); bottom: max(16px, env(safe-area-inset-bottom, 0px)); z-index: 10;
      max-width: min(420px, calc(100vw - 32px)); display: flex; align-items: flex-start; gap: 12px; padding: 12px 14px; font-size: 13px;
      background: var(--surface); color: var(--ink); border: 1px solid var(--line); border-radius: 12px; box-shadow: 0 12px 40px rgba(0, 0, 0, .35);
      animation: rise .18s ease-out; }
    @keyframes rise { from { transform: translateY(8px); opacity: 0; } }
    @media (prefers-reduced-motion: reduce) { .toast { animation: none; } }
    .toast span { flex: 1; min-width: 0; }
    .toast button { font: inherit; font-size: 15px; font-weight: 700; line-height: 1; border: 0; padding: 2px 4px; cursor: pointer; flex: none;
      background: var(--spectrum); -webkit-background-clip: text; background-clip: text; color: transparent; }
    .toast button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; border-radius: 4px; }
    .menu .item:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  `];

  constructor() {
    super();
    this.status = null; this.channels = []; this.reachable = true; this.access = null;
    this.view = location.hash === "#activity" ? "activity" : "channels";
    this.addEventListener("toast", (e) => this.notify(e.detail));
  }

  // A popover, so it shows above the source picker's modal dialog too.
  async notify(text) {
    clearTimeout(this.noteTimer);
    this.note = text;
    this.noteTimer = setTimeout(() => { this.note = null; }, 6000);
    await this.updateComplete;
    const el = this.renderRoot.querySelector(".toast");
    if (el?.matches(":popover-open")) el.hidePopover();
    el?.showPopover();
  }

  connectedCallback() {
    super.connectedCallback();
    this.onHash = () => { this.view = location.hash === "#activity" ? "activity" : "channels"; };
    window.addEventListener("hashchange", this.onHash);
    this.refresh();
    fetch("api/access").then((r) => r.ok ? r.json() : null).then((a) => { this.access = a; }).catch(() => {});
    // Only while the page is in view, and at once on coming back, so a hidden tab costs the Pi nothing.
    this.timer = setInterval(() => { if (!document.hidden) this.refresh(); }, REFRESH_MS);
    this.onVisible = () => { if (!document.hidden) this.refresh(); };
    document.addEventListener("visibilitychange", this.onVisible);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    window.removeEventListener("hashchange", this.onHash);
    clearInterval(this.timer);
    document.removeEventListener("visibilitychange", this.onVisible);
  }

  async refresh() {
    try {
      const [status, channels] = await Promise.all([
        fetch("api/status").then((r) => r.json()), fetch("api/channels").then((r) => r.json())]);
      this.status = status; this.channels = channels.channels || []; this.reachable = true;
    } catch {
      this.reachable = false;
    }
  }

  // A popover sits in the top layer, so it is placed under its button by hand.
  placeMenu(e) {
    if (e.newState !== "open") return;
    const b = this.renderRoot.querySelector(".me").getBoundingClientRect();
    Object.assign(e.target.style, { top: `${b.bottom + 8}px`, right: `${Math.max(16, innerWidth - b.right)}px` });
  }

  share() {
    this.renderRoot.querySelector("#me-menu").hidePopover();
    this.renderRoot.querySelector("share-dialog").open();
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
          ${this.access?.enabled ? html`<button class="me" popovertarget="me-menu" aria-label=${this.access.me ? `Account: ${this.access.me}` : "Not signed in"}>${avatar(this.access.me)}</button>
            <div class="menu" id="me-menu" popover @toggle=${(e) => this.placeMenu(e)}>
              ${this.access.me
                ? html`<div class="who">${avatar(this.access.me)}<span>${this.access.me}</span></div>
                  ${this.access.admin ? html`<button class="item" @click=${() => this.share()}>Share…</button>` : nothing}
                  <a class="item" href="/cdn-cgi/access/logout">Sign out</a>`
                : html`<div class="who">${avatar("")}<span>Not signed in.</span></div>`}
            </div>` : nothing}
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
        ? html`<activity-view .status=${s} .channels=${this.channels} .reload=${() => this.refresh()}></activity-view>`
        : html`<channel-board .channels=${this.channels} .jellyfin=${s?.jellyfin_playing || []} .reload=${() => this.refresh()}></channel-board>`}
      <share-dialog .access=${this.access} heading="bifrost ${s?.name ?? ""}"></share-dialog>
      ${this.note ? html`<div class="toast" popover="manual" role="status"><span>${this.note}</span><button aria-label="Dismiss" @click=${() => { this.note = null; }}>✕</button></div>` : nothing}
      <p class="note">Nothing on this page opens a channel: states come from real opens since bifrost last started, and from what Jellyfin reports about its own sessions. A channel with several sources skips a dead one by itself. Refreshes every 3 s while in view.</p>
    </div>`;
  }
}
customElements.define("bifrost-app", BifrostApp);
