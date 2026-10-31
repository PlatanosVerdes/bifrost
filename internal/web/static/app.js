// bifrost's status page: the app element fetches, every component below renders what it is given.
import { LitElement, html, svg, css, nothing, keyed } from "./vendor/lit-all.min.js";
import { lang, t, plural } from "./i18n.js";

document.documentElement.lang = lang;

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

const known = (at) => { const d = new Date(at); return !isNaN(d) && d.getFullYear() > 2000 ? d : null; };
const hhmm = (at) => known(at)?.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }) ?? "";
const ago = (at) => {
  const d = known(at);
  if (!d) return t("never");
  const s = Math.max(0, Math.round((Date.now() - d) / 1000));
  if (s < 60) return t("{s} s ago", { s });
  if (s < 3600) return t("{m} min ago", { m: Math.round(s / 60) });
  return t("{h} h {m} min ago", { h: Math.floor(s / 3600), m: Math.round((s % 3600) / 60) });
};
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
    ? t("The engine downloads {down} KB/s of a {rate} KB/s stream ({x}x)", { down, rate, x: sw.headroom.toFixed(2) }) + (sw.starving ? t(", short for a while: its buffer is draining") : "")
    : t("Engine download; the bitrate is known after a few seconds of playing");
  return html` · ${speedsOnly ? nothing : `${plural(sw.peers, "{n} peer", "{n} peers")} · `}<span class=${swarmTone(sw)} title=${title}>${rate ? `${down}/${rate} KB/s` : `${down} KB/s`}</span>${sw.stalls ? ` · ${plural(sw.stalls, "{n} stall", "{n} stalls")}` : ""}`;
};

function channelState(channel, jellyfin) {
  if (channel.playing) {
    // Who, and the swarm only when it is not keeping up: the rest is on Activity.
    const who = jellyfin.filter((p) => p.channel === channel.name).map((p) => p.user).filter(Boolean).join(", ");
    const sw = channel.playing.swarm;
    if (channel.playing.waiting) return { tone: "live", text: html`${t("Waiting for the broadcast")} · ${who || plural(channel.playing.viewers, "{n} viewer", "{n} viewers")}` };
    return { tone: "live", text: html`${t("Playing")} · ${who || plural(channel.playing.viewers, "{n} viewer", "{n} viewers")}${swarmTone(sw) ? swarm(sw, true) : nothing}` };
  }
  const o = channel.last_open;
  if (!o) return { tone: "none", text: t("Not opened yet") };
  if (o.outcome === "playing") return { tone: "played", text: `${t(o.device ? "Played at {time} on {device}" : "Played at {time}", { time: hhmm(o.at), device: o.device })} · ${t("first video in {s}", { s: secs(o.seconds) })}` };
  if (o.outcome === "gave_up") return { tone: "bad", text: t("No source played at {time} ({s})", { time: hhmm(o.at), s: secs(o.seconds) }) };
  return { tone: "warn", text: t("Left before it started at {time} ({s})", { time: hhmm(o.at), s: secs(o.seconds) }) };
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
  /* A new picture fades in; keyed() makes each one a new element, so this runs once per picture. */
  img.fresh { animation: fadein .45s ease-out; }
  details.more summary { display: inline-flex; align-items: center; gap: 6px; cursor: pointer; list-style: none; color: var(--muted); font-size: 12px; margin-top: 4px; }
  details.more summary::-webkit-details-marker { display: none; }
  details.more summary svg { width: 14px; height: 14px; }
  details.more summary:hover, details.more[open] summary { color: var(--ink); }
  details.more summary:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; border-radius: 4px; }
  details.more p { margin: 6px 0 0; font-size: 13px; color: var(--muted); max-width: 62ch; }
  @keyframes fadein { from { opacity: 0; } }
  .scan::after { content: ""; position: absolute; inset: 0; pointer-events: none;
    background: linear-gradient(100deg, transparent 30%, color-mix(in srgb, #fff 22%, transparent) 50%, transparent 70%) 0 0 / 250% 100%;
    animation: scan 1.4s linear infinite; }
  @keyframes scan { from { background-position: 125% 0; } to { background-position: -125% 0; } }
  @media (prefers-reduced-motion: reduce) { img.fresh, .scan::after { animation: none; } }
  .toast { position: fixed; inset: auto; margin: 0; left: max(16px, env(safe-area-inset-left, 0px)); bottom: max(16px, env(safe-area-inset-bottom, 0px)); z-index: 10;
    max-width: min(420px, calc(100vw - 32px)); display: flex; align-items: flex-start; gap: 12px; padding: 12px 14px; font-size: 13px;
    background: var(--surface); color: var(--ink); border: 1px solid var(--line); border-radius: 12px; box-shadow: 0 12px 40px rgba(0, 0, 0, .35);
    animation: rise .18s ease-out; }
  @keyframes rise { from { transform: translateY(8px); opacity: 0; } }
  @media (prefers-reduced-motion: reduce) { .toast { animation: none; } }
  .toast span { flex: 1; min-width: 0; }
  .toast button { font: inherit; font-size: 15px; font-weight: 700; line-height: 1; border: 0; padding: 2px 4px; cursor: pointer; flex: none;
    background: var(--spectrum); -webkit-background-clip: text; background-clip: text; color: transparent; }
  .toast button.act { font-size: 13px; font-weight: 600; background: none; color: var(--accent); }
  .toast button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; border-radius: 4px; }
`;

const switchable = (c) => c && c.ids.length > 1 && c.playing && !c.playing.waiting;
// A rainbow filled up to done of total, the half of the source being opened counted.
const progress = (done, total) => `--done: ${Math.min(100, Math.round((100 * (done + 0.5)) / total))}%`;
// A source with no picture yet: a struck-out screen, in the page's muted ink.
const noPicture = svg`<svg class="none" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="5" width="18" height="12" rx="2"/><path d="M8 21h8M12 17v4M4 4l16 16"/></svg>`;
// Went back to play a stretch again in the last 15 minutes, as bifrost ranks it.
const looping = (x) => x.loops > 0 && Date.now() - Date.parse(x.looped_at) < 15 * 60 * 1000;
const info = svg`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/></svg>`;
const calm = () => matchMedia("(prefers-reduced-motion: reduce)").matches;
const thumbSrc = (id, at) => `api/thumbs/${id}?t=${Date.parse(at)}`;
// A toast hides by itself, but not while the pointer or focus is on it, so its action stays reachable.
class Toasts {
  constructor(host) { this.host = host; this.note = null; }
  show(note) { this.note = { ...note, ms: note.action ? 8000 : 6000 }; this.arm(); this.host.requestUpdate(); }
  arm() { clearTimeout(this.timer); if (this.note) this.timer = setTimeout(() => this.close(), this.note.ms); }
  hold() { clearTimeout(this.timer); }
  close() { clearTimeout(this.timer); this.note = null; this.host.requestUpdate(); }
  view(popover) {
    const n = this.note;
    if (!n) return nothing;
    return html`<div class="toast" popover=${popover ? "manual" : nothing} role="status" @pointerenter=${() => this.hold()} @pointerleave=${() => this.arm()}
      @focusin=${() => this.hold()} @focusout=${() => this.arm()}><span>${n.text}</span>${n.action
        ? html`<button class="act" @click=${() => { const run = n.action.run; this.close(); run(); }}>${n.action.label}</button>` : nothing}<button aria-label=${t("Dismiss")} @click=${() => this.close()}>✕</button></div>`;
  }
}
const toast = (from, text, action) => from.dispatchEvent(new CustomEvent("toast", { detail: { text, action }, bubbles: true, composed: true }));
const playIcon = svg`<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M7 4.5v15a1 1 0 0 0 1.5.86l12.5-7.5a1 1 0 0 0 0-1.72L8.5 3.64A1 1 0 0 0 7 4.5z"/></svg>`;
const spinner = svg`<svg class="spin" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true"><path d="M12 3a9 9 0 1 0 9 9"/></svg>`;
const trash = svg`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 7h16M10 11v6M14 11v6M5 7l1 12a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2l1-12M9 7V4h6v3"/></svg>`;

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
    .panel li.line { display: grid; grid-template-columns: 104px minmax(0, 1fr) auto; gap: 4px 14px; align-items: center; padding: 10px 12px; font-size: 13px; }
    /* The source playing: tinted, with a bar on its edge and on air marked on its picture. */
    .line.on { background: color-mix(in srgb, var(--accent) 8%, transparent); box-shadow: inset 3px 0 0 var(--accent); }
    .line.dead .thumb, .line.dead .name { opacity: .55; }
    .line .thumb { position: relative; grid-row: 1 / 3; width: 104px; aspect-ratio: 16 / 9; border-radius: 6px; overflow: hidden; background: var(--chip); border: 0; padding: 0; display: grid; place-items: center; font-size: 10px; color: var(--muted); }
    .line button.thumb { cursor: zoom-in; }
    .line .thumb img { width: 100%; height: 100%; object-fit: cover; display: block; }
    .line span.thumb { background: var(--stripes); color: var(--muted); }
    .line .thumb svg.none { width: 22px; height: 22px; opacity: .8; }
    .line .info { grid-column: 2 / 3; font-size: 12px; color: var(--muted); font-variant-numeric: tabular-nums; }
    .info > * + * { margin-left: 10px; }
    .id { font-family: var(--font-data); font-size: 11px; color: var(--muted); margin-left: 8px; user-select: all; }
    .acts { display: flex; gap: 6px; justify-self: center; }
    .onair { position: absolute; left: 5px; bottom: 5px; display: inline-flex; align-items: flex-end; gap: 2px; padding: 3px 6px 3px 5px; border-radius: 4px;
      background: rgba(0, 0, 0, .7); color: #fff; font-size: 10px; font-weight: 600; line-height: 1; }
    .onair i { width: 2px; height: 8px; border-radius: 1px; background: var(--ok); animation: eq .9s ease-in-out infinite; }
    .onair i:nth-child(2) { animation-delay: -.3s; } .onair i:nth-child(3) { animation-delay: -.6s; }
    .onair span { margin-left: 4px; }
    @keyframes eq { 0%, 100% { height: 3px; } 50% { height: 9px; } }
    @media (prefers-reduced-motion: reduce) { .onair i { animation: none; } }
    .play { display: grid; place-items: center; width: 32px; height: 32px; padding: 0; border: 0; border-radius: 50%; cursor: pointer;
      color: var(--accent); background: color-mix(in srgb, var(--accent) 14%, transparent); transition: background .15s, color .15s; }
    .play svg { width: 14px; height: 14px; }
    .play:hover:not(:disabled), .play:focus-visible { color: var(--surface); background: var(--accent); }
    .play:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .play:disabled { cursor: default; opacity: .5; }
    .play .spin { animation: spin .8s linear infinite; }
    @keyframes spin { to { transform: rotate(360deg); } }
    /* Same box as the play button, both centred in the last column, so the two icons line up. */
    .line .remove { grid-column: 3; justify-self: center; display: grid; place-items: center; width: 32px; height: 32px; padding: 0;
      border: 0; border-radius: 6px; background: none; color: var(--muted); cursor: pointer; opacity: .7; }
    .line .remove svg { width: 16px; height: 16px; }
    .line .remove:hover, .line .remove:focus-visible { opacity: 1; color: var(--calm-bad); background: color-mix(in srgb, var(--calm-bad) 10%, transparent); }
    .line .remove:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .line .remove:disabled { visibility: hidden; }
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
    .tag { font-size: 11px; font-weight: 600; border-radius: 5px; padding: 1px 7px; margin-left: 6px; color: var(--muted); background: var(--chip); }
    .tag.on { background: color-mix(in srgb, var(--accent) 16%, transparent); color: var(--accent); }
  `];

  constructor() { super(); this.toasts = new Toasts(this); }

  // A modal dialog makes everything outside it inert, so while the sheet is open its toasts show inside it.
  say(text, action) {
    if (this.renderRoot.querySelector("dialog")?.open) this.toasts.show({ text, action });
    else toast(this, text, action);
  }

  // Rows glide to their new place when the order changes, instead of jumping.
  willUpdate() {
    this.places = new Map([...this.renderRoot?.querySelectorAll?.("li.line[data-id]") ?? []].map((li) => [li.dataset.id, li.getBoundingClientRect().top]));
  }

  updated() {
    if (!this.places?.size || calm()) return;
    for (const li of this.renderRoot.querySelectorAll("li.line[data-id]")) {
      const was = this.places.get(li.dataset.id), dy = was == null ? 0 : was - li.getBoundingClientRect().top;
      if (Math.abs(dy) > 2) li.animate([{ transform: `translateY(${dy}px)` }, { transform: "none" }], { duration: 280, easing: "cubic-bezier(.2, .8, .2, 1)" });
    }
  }

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
        .catch(() => { throw new Error(t("Could not reach bifrost")); });
      if (!r.ok) {
        const body = await r.json().catch(() => null);
        throw new Error(body ? body.error || t("Source {n} did not start", { n: x.n }) : t("No answer from bifrost (HTTP {status})", { status: r.status }));
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
        .catch(() => { throw new Error(t("Could not reach bifrost")); });
      if (!r.ok) {
        const body = await r.json().catch(() => null);
        throw new Error(body?.error || t("No answer from bifrost (HTTP {status})", { status: r.status }));
      }
      await this.reload?.();
      this.edit = null;
      if (removed) this.say(t("Source {n} removed from the channel", { n: x.n }), { label: t("Undo"), run: () => this.block(x, false) });
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
      const r = await fetch(`api/channels/${c.slug}/survey`, { method: "POST" }).catch(() => { throw new Error(t("Could not reach bifrost")); });
      const body = await r.json().catch(() => null);
      if (!r.ok) throw new Error(body?.error || t("No answer from bifrost (HTTP {status})", { status: r.status }));
      if (!body.checking) this.say(t("Every source of this channel was checked a moment ago"));
      while (body.checking) {
        await new Promise((ok) => setTimeout(ok, 2000));
        await this.reload?.();
        if (!this.channel.survey) break;
      }
    } catch (e) {
      this.say(e.message);
    }
    this.check = null;
  }

  surveyNote() {
    const sv = this.channel.survey;
    if (sv) return html`<span class="checking" role="status"><span class="bar" style=${progress(sv.done, sv.total)}></span>${t("{done} of {total}", { done: sv.done + 1, total: sv.total })}</span>`;
    return html`<button class="link" ?disabled=${this.check?.busy} @click=${() => this.survey()}>${t("Check sources")}</button>`;
  }

  thumb(x, on = false, scanning = false) {
    const onair = on ? html`<span class="onair"><i></i><i></i><i></i><span>${t("On air")}</span></span>` : nothing;
    const cls = `thumb ${scanning ? "scan" : ""}`;
    if (!known(x.thumb_at)) return html`<span class=${cls} title=${scanning ? t("Checking") : t("No picture yet: Check sources or play it")}>${noPicture}${onair}</span>`;
    const src = thumbSrc(x.id, x.thumb_at);
    return html`<button class=${cls} aria-label=${t("Enlarge the picture of {id}", { id: x.id.slice(0, 8) })} @click=${() => { this.zoom = { ...x, src }; }}>${keyed(src, html`<img class="fresh" src=${src} alt="" loading="lazy">`)}${onair}</button>`;
  }

  // Numbered in the playlist's order, so a source keeps its number whichever plays.
  ranked() {
    const c = this.channel;
    return (c.sources?.length ? c.sources : c.ids.map((id) => ({ id }))).map((x, i) => ({ ...x, n: i + 1 }))
      .sort((a, b) => (a.outcome === "failed") - (b.outcome === "failed") || looping(a) - looping(b) || (b.headroom ?? -1) - (a.headroom ?? -1));
  }

  facts(x) {
    const kept = x.kept ? html`<span class="muted">${t("Not in the list")}</span>` : "";
    if (!x.seen_at && x.headroom == null) return [kept, html`<span class="muted">${t("Not tried yet")}</span>`].filter(Boolean);
    const tone = x.headroom == null ? "" : swarmTone({ headroom: x.headroom }) || "quick";
    const loops = looping(x) ? html`<span class="warn" title=${t("It went back to play a stretch again; bifrost drops the repeat and tries this source after the rest")}>${plural(x.loops, "{n} loop", "{n} loops")}</span>` : "";
    const bits = [kept, x.outcome === "failed" ? html`<span class="bad">${t("Failed")}</span>` : "", loops, x.headroom != null ? html`<span class=${tone}>${x.headroom.toFixed(2)}x</span>` : "",
      x.height ? `${x.height}p` : "", x.audio, x.peers != null ? plural(x.peers, "{n} peer", "{n} peers") : "", hhmm(x.seen_at)].filter(Boolean);
    return bits.map((b) => html`<span>${b}</span>`);
  }

  quick() {
    const pick = this.pick, next = this.ranked().find((x) => x.id !== this.channel.playing.source && x.outcome !== "failed");
    if (pick?.id && !pick.error) return html`<span class="muted">${t("Switching…")}</span>`;
    if (pick?.error) return html`<span class="bad">${pick.error}</span>`;
    if (pick?.done) return html`<span class="muted">${t("Switched to Source {n}", { n: pick.done })}</span>`;
    return next ? html`<button class="link" @click=${() => this.choose(next)}>${t("Switch to best")}</button>` : nothing;
  }

  render() {
    const c = this.channel, live = switchable(c);
    const quick = live ? this.quick() : nothing;
    return html`${this.count ? html`<button class="src" aria-haspopup="dialog" @click=${() => this.show()}>${plural(c.ids.length, "{n} source", "{n} sources")}</button>` : nothing}${quick === nothing ? nothing : html`<span class="gap">${quick}</span>`}
    ${this.opened ? this.sheet(c, live) : nothing}`;
  }

  sheet(c, live) {
    const pick = this.pick, pending = pick?.id && !pick.error;
    return html`<dialog aria-label=${t("Sources of {name}", { name: c.name })} @click=${(e) => { if (e.target === e.currentTarget) e.target.close(); }} @close=${() => { this.zoom = null; this.toasts.close(); }}>
      <div class="sheet" tabindex="-1" autofocus><button class="x" aria-label=${t("Close")} @click=${(e) => e.target.closest("dialog").close()}>✕</button><h3>${c.name}</h3><div class="head"><p>${live ? t("Pick a source for this channel. Everyone watching it switches too.") : t("Not playing now: a source can be picked while it plays.")}</p>${this.surveyNote()}</div>
        <ul class="panel">${this.ranked().map((x, i) => {
          const on = x.id === c.playing?.source, mine = pick?.id === x.id, edit = this.edit?.id === x.id ? this.edit : null;
          return html`<li class="line ${on ? "on" : ""} ${x.outcome === "failed" ? "dead" : ""}" data-id=${x.id}>
            ${this.thumb(x, on, c.survey?.current === x.id)}
            <span class="name"><b>${t("Source {n}", { n: x.n })}</b><span class="id" title=${x.id}>${x.id.slice(0, 8)}</span>${i === 0 && x.headroom != null && x.outcome !== "failed" ? html`<span class="tag">${t("Best")}</span>` : nothing}${mine && pick.error ? html` <span class="bad">${pick.error}</span>` : nothing}${edit?.error ? html` <span class="bad">${edit.error}</span>` : nothing}</span>
            <span class="acts">${!on && live ? html`<button class="play" title=${t("Play source {n}", { n: x.n })} aria-label=${mine && pending ? t("Switching…") : t("Play source {n}", { n: x.n })} ?disabled=${pending} @click=${() => this.choose(x)}>${mine && pending ? spinner : playIcon}</button>` : nothing}</span>
            <span class="info">${this.facts(x)}</span>
            ${on ? nothing : html`<button class="remove" title=${t("Remove from this channel")} aria-label=${t("Remove source {n} from this channel", { n: x.n })} ?disabled=${pending || edit || c.ids.length < 2} @click=${() => this.block(x, true)}>${trash}</button>`}</li>`;
        })}</ul>
        ${c.blocked?.length ? html`<details class="removed"><summary>${plural(c.blocked.length, "{n} removed source", "{n} removed sources")}</summary>
          <ul class="panel">${c.blocked.map((x) => {
            const edit = this.edit?.id === x.id ? this.edit : null;
            return html`<li class="line">${this.thumb(x)}
              <span><span class="id" title=${x.id}>${x.id.slice(0, 8)}</span>${edit?.error ? html` <span class="bad">${edit.error}</span>` : nothing}</span>
              <span class="acts"><button class="ctl" ?disabled=${edit} @click=${() => this.block(x, false)}>${t("Restore")}</button></span>
              <span class="info">${this.facts(x)}</span></li>`;
          })}</ul></details>` : nothing}
        ${this.zoom ? html`<div class="zoom" @click=${() => { this.zoom = null; }}><img src=${this.zoom.src} alt=${t("Picture of {id}", { id: this.zoom.id.slice(0, 8) })}><p class="muted"><span class="id" title=${this.zoom.id}>${this.zoom.id}</span> ${hhmm(this.zoom.thumb_at)}</p></div>` : nothing}${this.toasts.view(false)}</div>
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
    return html`<button class="src" aria-haspopup="dialog" @click=${(e) => e.target.closest("li").querySelector("source-picker").show()}>${plural(c.ids.length, "{n} source", "{n} sources")}</button>`;
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
          ${c.now ? html`<span class="air" title=${t("{title}, until {time}", { title: c.now.title, time: hhmm(c.now.stop) })}>${c.now.title} · ${t("until {time}", { time: hhmm(c.now.stop) })}</span>` : nothing}
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
      const group = c.group || t("Other");
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
        <input type="search" placeholder=${t("Search channels…")} aria-label=${t("Search channels")} .value=${this.query} @input=${(e) => { this.query = e.target.value; }}>
        <div class="legend" role="group" aria-label=${t("Show first")}>
          ${[["live", t("Playing")], ["played", t("Last open played")], ["warn", t("Last open left early")], ["bad", t("Last open failed")], ["none", t("Not opened yet")]].map(([tone, label]) =>
            html`<button aria-pressed=${this.first === tone} @click=${() => { this.first = this.first === tone ? null : tone; }}><i class="dot ${tone}"></i>${label}</button>`)}
        </div>
      </div>
      ${groups.length
        ? html`<div class="groups">${groups.map((g) => html`<channel-group .name=${g.name} .channels=${g.channels} .jellyfin=${this.jellyfin} .reload=${this.reload}></channel-group>`)}</div>`
        : html`<p class="empty">${this.channels.length ? t("No channel matches.") : t("No channel list yet.")}</p>`}`;
  }
}
customElements.define("channel-board", ChannelBoard);

class ActivityView extends LitElement {
  static properties = { status: { attribute: false }, channels: { attribute: false }, reload: { attribute: false }, zoom: { state: true } };
  static styles = [base, css`
    .row { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 14px; align-items: center; }
    .row > div { min-width: 0; overflow-wrap: anywhere; }
    .row + .row { margin-top: 12px; }
    .devices { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; }
    .devices li { display: grid; grid-template-columns: none; padding: 0; border: 0; }
    .ring { width: 44px; height: 44px; border-radius: 50%; border: 2px dashed var(--line); display: grid; place-items: center; color: var(--muted); }
    .ring.on { border: 2px solid var(--accent); color: var(--accent); }
    .row p, .title p { margin: 2px 0 0; color: var(--muted); font-size: 14px; max-width: 62ch; }
    .row p.air, .title p.air { color: var(--ink); font-size: 15px; font-weight: 500; }
    .row p.air span, .title p.air span { color: var(--muted); font-weight: 400; }
    .stream { display: grid; grid-template-columns: minmax(0, 160px) minmax(0, 1fr); gap: 14px 20px; align-items: center; }
    @media (max-width: 720px) { .stream { grid-template-columns: minmax(0, 1fr); } }
    .title { min-width: 0; overflow-wrap: anywhere; }
    .shot { position: relative; width: 100%; aspect-ratio: 16 / 9; border-radius: 10px; overflow: hidden; background: var(--chip); border: 0; padding: 0; display: block; }
    button.shot { cursor: zoom-in; }
    button.shot:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    .shot img { width: 100%; height: 100%; object-fit: cover; display: block; }
    .shot.empty { display: grid; place-content: center; justify-items: center; gap: 8px; padding: 16px; text-align: center; font-size: 12px; color: var(--muted);
      background: var(--stripes); }
    .shot.empty svg { width: 32px; height: 32px; opacity: .8; }
    .shot .at { position: absolute; left: 8px; bottom: 8px; font-family: var(--font-data); font-size: 11px; color: #fff; background: rgba(0, 0, 0, .6); border-radius: 5px; padding: 1px 6px; }
    dialog.zoom { width: min(960px, calc(100vw - 32px)); padding: 0; border: 0; border-radius: 12px; background: #000; cursor: zoom-out; }
    dialog.zoom::backdrop { background: rgba(0, 0, 0, .7); }
    dialog.zoom img { width: 100%; display: block; }
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
    .tag { font-size: 11px; font-weight: 600; border-radius: 5px; padding: 1px 7px; margin-left: 6px; vertical-align: 1px; display: inline-block; white-space: nowrap; overflow-wrap: normal; }
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

  // The playing source's latest picture, cut every couple of minutes from what already streams.
  shot(st) {
    if (!known(st.thumb_at)) return html`<div class="shot empty">${noPicture}<span>${t("No picture yet, it shows some 20 s after the source starts")}</span></div>`;
    const src = thumbSrc(st.source, st.thumb_at), alt = t("What {channel} showed at {time}", { channel: st.channel, time: hhmm(st.thumb_at) });
    return html`<button class="shot" aria-label=${t("Enlarge: {alt}", { alt })} @click=${() => this.enlarge({ src, alt })}>${keyed(src, html`<img class="fresh" src=${src} alt="">`)}<span class="at">${hhmm(st.thumb_at)}</span></button>`;
  }

  async enlarge(zoom) {
    this.zoom = zoom;
    await this.updateComplete;
    this.renderRoot.querySelector("dialog.zoom")?.showModal();
  }

  screen(on) {
    return html`<div class="ring ${on ? "on" : ""}" aria-hidden="true"><svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><rect x="3" y="5" width="18" height="12" rx="2"/><path d="M8 21h8"/></svg></div>`;
  }

  now() {
    const streams = this.status.streams || [], jellyfin = this.status.jellyfin_playing || [];
    if (!streams.length) {
      return html`<div class="row">${this.screen(false)}<div><b>${t("Nobody is watching")}</b>
        <details class="more"><summary>${info}<span>${t("What shows here")}</span></summary><p>${t("When a channel is open it shows here: what it airs by the guide, its source, how many are watching, how soon the engine gave the first video, its peers and whether it downloads faster than the channel plays, whether Jellyfin copies the video or re-encodes it, and whether it keeps up with live.")}</p></details></div></div>`;
    }
    return html`<div class="scroller">${streams.map((st) => this.stream(st, jellyfin))}</div>`;
  }

  stream(st, jellyfin) {
    const watchers = jellyfin.filter((p) => st.channel.startsWith(p.channel));
    const jf = watchers[0];
    const transcoding = jf && jf.video !== "copy";
    return html`<div class="stream">${this.shot(st)}<div class="body"><div class="title">
      <b>${st.channel}</b>${jf ? (transcoding
        ? html`<span class="tag trans" title=${t("Jellyfin re-encodes the video on the Pi")}>${t("Re-encoding")}</span>`
        : html`<span class="tag copy" title=${t("Jellyfin passes the video through untouched")}>${t("Direct")}</span>`) : nothing}${transcoding && jf.speed && jf.speed < 0.95 ? html`<span class="tag slow" title=${t("Jellyfin's re-encode against live: below 1x the picture stalls")}>${t("Falling behind")} · ${jf.speed.toFixed(2)}x</span>` : nothing}${st.probing ? html`<span class="tag probe" title=${t("Measuring another source of the channel next to this one, to move to it if it is clearly better")}>${t("Trying {id}…", { id: st.probing_source.slice(0, 8) })}</span>` : nothing}
      ${st.now ? html`<p class="air">${st.now.title} <span>${t(st.next ? "until {time}, then {next}" : "until {time}", { time: hhmm(st.now.stop), next: st.next?.title })}</span></p>` : nothing}
    </div>
    <div class="facts">
      ${this.watching(st, watchers)}
      ${this.swarmFact(st.swarm)}
      <div class="fact"><span class="lbl">${t("Source")}</span>${this.source(st.source)}
        <span class="sub">${t("since {time}, first video in {s}", { time: hhmm(st.since), s: secs(st.started_after_s) })}</span>
        ${switchable(this.channels?.find((c) => c.slug === st.slug)) ? html`<span class="sub"><source-picker count .channel=${this.channels.find((c) => c.slug === st.slug)} .reload=${this.reload}></source-picker></span>` : nothing}
        ${transcoding && jf.reasons ? html`<span class="sub">${t("Jellyfin re-encodes: {reasons}", { reasons: jf.reasons.join(", ") })}</span>` : nothing}</div>
    </div>
    </div></div>`;
  }

  // A Jellyfin connection no session plays is one Jellyfin left open (a zap, a TV switched off), not a viewer.
  watching(st, watchers) {
    const external = st.external ?? 0, leftOpen = Math.max(0, st.viewers - external - watchers.length);
    const notes = [external > 0 ? html`<li class="muted">${plural(external, "{n} viewer not through Jellyfin", "{n} viewers not through Jellyfin")}</li>` : "",
      leftOpen > 0 ? html`<li class="muted">${t("Left open by Jellyfin, closes by itself")}</li>` : ""].filter(Boolean);
    if (!watchers.length && !external) return html`<div class="fact"><span class="lbl">${t("Watching")}</span>${t("Nobody")}${leftOpen ? html`<span class="sub">${t("Left open by Jellyfin, closes by itself")}</span>` : nothing}</div>`;
    // One line per device: the same user on two screens is two lines, not one name.
    return html`<div class="fact"><span class="lbl">${t("Watching")}</span><ul class="devices">${watchers.map((w) => html`<li>${w.user || w.device}<span class="sub">${[w.device, w.client].filter(Boolean).join(" · ")}</span></li>`)}${notes}</ul></div>`;
  }

  // The headroom bar is the loud thing on the card: the tick is what the stream needs (1x), the bar fills to 1.5x.
  swarmFact(sw) {
    if (!sw?.sampled_at) return html`<div class="fact"><span class="lbl">${t("Swarm")}</span><span class="muted">${t("Measuring…")}</span></div>`;
    const down = kb(sw.download_bytes_per_second), rate = kb(sw.bitrate_bytes_per_second), tone = swarmTone(sw);
    const fill = rate ? Math.min(sw.headroom / 1.5, 1) * 100 : 0;
    return html`<div class="fact"><span class="lbl">${t("Swarm")}</span>${plural(sw.peers, "{n} peer", "{n} peers")}
      ${rate ? html`<div class="gauge ${tone}" role="img" aria-label=${t("{x} times what the stream needs", { x: sw.headroom.toFixed(2) })}><i style="width:${fill}%"></i></div>
        <span class="sub ${tone}">${t("{down} KB/s of {rate} needed", { down, rate })} · ${sw.headroom.toFixed(2)}x${tone === "short" ? t(", buffer draining") : ""}</span>`
        : html`<span class="sub">${t("{down} KB/s, bitrate known in a few seconds", { down })}</span>`}
      ${sw.stalls ? html`<span class="sub">${plural(sw.stalls, "{n} stall", "{n} stalls")}</span>` : nothing}</div>`;
  }

  // The short id copies a link to that one source, for a player that is not Jellyfin, like VLC.
  source(id) {
    if (!id) return "";
    const copy = async (e) => {
      await navigator.clipboard.writeText(`${location.origin}/ace/getstream?id=${id}`);
      e.target.textContent = t("Link copied");
      setTimeout(() => { e.target.textContent = `${id.slice(0, 8)}…`; }, 1500);
    };
    return html`<button class="source mono" title=${t("{id} · click to copy a link VLC can play", { id })} @click=${copy}>${id.slice(0, 8)}…</button>`;
  }

  recent() {
    const recent = this.status.recent || [];
    if (!recent.length) return html`<li><span></span><span class="muted">${t("No opens since bifrost started.")}</span><span></span></li>`;
    return recent.map((r) => {
      // Grey is normal; colour only says something: under a second, slow, or nothing played.
      const tone = r.outcome === "gave_up" ? "bad" : r.outcome !== "playing" ? "" : r.seconds < 1 ? "quick" : r.seconds > 5 ? "tight" : "";
      const res = r.outcome === "playing" ? secs(r.seconds) : r.outcome === "gave_up" ? t("No video") : t("Left early");
      const title = t(r.outcome === "playing" ? "first video after {s}" : "gave up after {s}", { s: secs(r.seconds) });
      return html`<li><span class="at">${hhmm(r.at)}</span><span class="ch">${r.channel}</span><span class="res ${tone || "muted"}" title=${title}>${res}</span>
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
      <h2 class="first">${t("Now")}</h2>
      <section class="card">${this.now()}</section>
      ${this.zoom ? html`<dialog class="zoom" aria-label=${this.zoom.alt} @click=${(e) => e.currentTarget.close()} @close=${() => { this.zoom = null; }}><img src=${this.zoom.src} alt=${this.zoom.alt}></dialog>` : nothing}
      <div class="duo">
        <div><h2>${t("Recent opens")}</h2><section class="card"><ul class="scroller">${this.recent()}</ul></section></div>
        <div><h2>${t("List and guide")}</h2><section class="card">
          <div class="big"><div><b>${l.channels ?? "–"}</b><span>${t("channels")}</span></div><div><b>${l.sources ?? "–"}</b><span>${t("sources")}</span></div><div><b>${g.channels ?? "–"}</b><span>${t("with guide")}</span></div></div>
          <dl>
            <dt>${t("List")}</dt><dd>${known(l.last_change) ? t("read {ago}; changed {changed}", { ago: ago(l.last_read), changed: ago(l.last_change) }) : t("read {ago}", { ago: ago(l.last_read) })}${l.error ? html` · <span class="bad">${l.error}</span>` : nothing}</dd>
            <dt>${t("Guide")}</dt><dd>${g.bytes ? t("{kb} KB for {n} channels", { kb: Math.round(g.bytes / 1024), n: g.channels }) : t("not cut yet")}</dd>
            <dt>Jellyfin</dt>${j.last_code
              ? j.last_code === 204
                ? html`<dd class="ok">${t("reread the guide {ago}", { ago: ago(j.last_refresh) })}</dd>`
                : html`<dd class="bad">${t("guide refresh failed {ago} (HTTP {code})", { ago: ago(j.last_refresh), code: j.last_code })}</dd>`
              : html`<dd>${t("no refresh asked yet")}</dd>`}
            ${j.refresh_due ? html`<dt>${t("Next")}</dt><dd>${t("refresh due at {time}", { time: hhmm(j.refresh_due) })}</dd>` : nothing}
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
    const date = new Date(i.at).toLocaleDateString([], { day: "numeric", month: "short" });
    return html`<small>${i.by === this.access.me?.toLowerCase() ? t("invited by you · {date}", { date }) : t("invited by {by} · {date}", { by: i.by, date })}</small>`;
  }

  members(group) {
    if (!group.members.length) return html`<li class="muted">${t("Nobody yet.")}</li>`;
    const me = this.access.me?.toLowerCase(), owners = this.data.owners ?? [], admins = this.data.admins ?? [];
    const isAdmin = (m) => owners.includes(m) || admins.includes(m);
    return group.members.map((m) => html`<li>${avatar(m)}
      <span class="who">${m}${m === me ? html`<small>${t("you · admin")}</small>` : this.invited(m)}</span>
      ${group.editable && m !== me ? html`${owners.includes(m)
        ? html`<span class="fixed" title=${t("Set in bifrost's settings")}>${t("Admin")}</span>`
        : html`<select class="role" aria-label=${t("Role of {email}", { email: m })} ?disabled=${this.busy} @change=${(e) => this.change("PATCH", { email: m, role: e.target.value })}>
            <option value="viewer" ?selected=${!isAdmin(m)}>${t("Viewer")}</option><option value="admin" ?selected=${isAdmin(m)}>${t("Admin")}</option></select>`}
        <button class="btn remove" ?disabled=${this.busy} @click=${() => this.change("DELETE", { group: group.id, email: m })}>${t("Remove")}</button>` : nothing}</li>`);
  }

  copyBox(text, label) {
    return html`<div class="copy"><input readonly .value=${text} aria-label=${label} @focus=${(e) => e.target.select()}>
      <button class="btn primary" @click=${() => this.copy(text)}>${this.copied ? t("Copied") : t("Copy")}</button></div>`;
  }

  joinSection(group) {
    const links = Object.entries(this.data.links ?? {}).sort(([, a], [, b]) => a.expires.localeCompare(b.expires));
    const day = (at) => new Date(at).toLocaleDateString([], { day: "numeric", month: "short" });
    return html`${this.created
        ? html`${this.copyBox(this.created, t("Invite link"))}<p class="hint">${t("Send it by any chat. It lets one person in, for a week: they sign in with Google or a code sent to their email and become a member.")}</p>`
        : html`<button class="btn primary" ?disabled=${this.busy} @click=${() => this.createLink(group)}>${t("Create invite link")}</button>
          <p class="hint">${t("Whoever opens it first gets in, so send it only to the person you mean.")}</p>`}
      ${links.length ? html`<h4>${t("Invite links not used yet")}</h4><ul>${links.map(([id, l]) => html`<li>
        <span class="who">${l.by === this.access.me?.toLowerCase() ? t("Made by you") : t("Made by {by}", { by: l.by })}<small>${t("works until {date}", { date: day(l.expires) })}</small></span>
        <button class="btn remove" ?disabled=${this.busy} @click=${() => this.change("DELETE", { id }, "api/access/links")}>${t("Revoke")}</button></li>`)}</ul>` : nothing}`;
  }

  render() {
    if (!this.access?.admin) return nothing;
    const a = this.data, editable = a?.groups.find((g) => g.editable);
    return html`<dialog aria-labelledby="share-title">
      <button class="btn close" aria-label=${t("Close")} @click=${() => this.renderRoot.querySelector("dialog").close()}>✕</button>
      <h3 id="share-title">${t("Share {name}", { name: this.heading })}</h3>
      <p class="lead">${t("People on this list can open this page after signing in with Google or a code sent to their email. They see the page only: streams never leave the house.")}</p>
      ${!a ? html`${this.error ? nothing : html`<p class="hint">${t("Loading who has access…")}</p>`}` : html`
      ${editable
        ? html`${a.joinable ? this.joinSection(editable) : nothing}
          <h4>${a.joinable ? t("Or add their email") : t("Add their email")}</h4>
          <form @submit=${(e) => this.invite(e, editable)}>
            <input type="text" inputmode="email" autocomplete="off" placeholder="user@example.com" aria-label=${t("Email addresses")}>
            <button class="btn ${a.joinable ? "" : "primary"}" ?disabled=${this.busy}>${t("Add")}</button></form>
          <p class="hint">${t("They must sign in with this exact address. Separate several with commas.")}</p>`
        : html`<p class="hint">${t("No list here can be edited from bifrost: each one may also let people into something else.")}</p>`}
      ${a.groups.map((g) => html`<h4>${g.editable ? t("Members") : t("{name} · managed elsewhere", { name: g.name })}</h4><ul>${this.members(g)}</ul>`)}
      ${editable ? html`<p class="hint">${t("Removing someone stops new sign-ins at once; a page they already have open keeps working until their session ends (up to a day).")}</p>` : nothing}`}
      ${this.error ? html`<p class="err" role="alert">${this.error}</p>` : nothing}
    </dialog>`;
  }
}
customElements.define("share-dialog", ShareDialog);

class BifrostApp extends LitElement {
  static properties = { status: { state: true }, channels: { state: true }, view: { state: true }, reachable: { state: true }, access: { state: true } };
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
    /* Healthy is a calm pulse with its word in the tooltip; anything else says what is wrong. */
    .dot.pulse { animation: pulse 2.4s ease-out infinite; }
    @keyframes pulse { 0% { box-shadow: 0 0 0 0 color-mix(in srgb, var(--ok) 55%, transparent); } 70%, 100% { box-shadow: 0 0 0 7px transparent; } }
    @media (prefers-reduced-motion: reduce) { .dot.pulse { animation: none; } }
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
    .menu .item:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  `];

  constructor() {
    super();
    this.status = null; this.channels = []; this.reachable = true; this.access = null;
    this.view = location.hash === "#activity" ? "activity" : "channels";
    this.toasts = new Toasts(this);
    this.addEventListener("toast", (e) => this.notify(e.detail));
  }

  // A popover, so it shows above the source picker's modal dialog too.
  async notify(note) {
    this.toasts.show(note);
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
    const health = !this.reachable ? t("Not answering") : healthy ? t("Healthy") : s ? t("No list yet") : t("Loading");
    const watching = (s?.streams || []).reduce((a, st) => a + st.viewers, 0);
    const tab = (id, label) => html`<a href="#${id}" aria-current=${this.view === id ? "page" : nothing}>${label}</a>`;
    return html`<div class="wrap">
      <header>
        <div class="brand"><p class="word">bifrost</p><span class="muted">${s?.name ?? ""} · ${location.host}</span></div>
        <div class="right">
          <span class="chip">${s?.version ?? "…"}</span>
          <span class="pill" title=${health} role="status" aria-label=${health}><span class="dot ${healthy ? "ok pulse" : s || !this.reachable ? "bad" : "none"}"></span>${healthy ? nothing : health}</span>
          ${this.access?.enabled ? html`<button class="me" popovertarget="me-menu" aria-label=${this.access.me ? t("Account: {email}", { email: this.access.me }) : t("Not signed in")}>${avatar(this.access.me)}</button>
            <div class="menu" id="me-menu" popover @toggle=${(e) => this.placeMenu(e)}>
              ${this.access.me
                ? html`<div class="who">${avatar(this.access.me)}<span>${this.access.me}</span></div>
                  ${this.access.admin ? html`<button class="item" @click=${() => this.share()}>${t("Share…")}</button>` : nothing}
                  <a class="item" href="/cdn-cgi/access/logout">${t("Sign out")}</a>`
                : html`<div class="who">${avatar("")}<span>${t("Not signed in.")}</span></div>`}
            </div>` : nothing}
        </div>
      </header>
      <div class="band" aria-hidden="true"></div>
      <div class="bar">
        <nav aria-label=${t("Views")}>${tab("channels", t("Channels"))}${tab("activity", t("Activity"))}</nav>
        <div class="summary">
          <span><b>${l.channels ?? 0}</b> ${t("channels")}</span><span><b>${l.sources ?? 0}</b> ${t("sources")}</span>
          <span>${t("list read")} <b>${ago(l.last_read)}</b></span><span>${watching ? html`<b>${watching}</b> ${t("watching")}` : t("nobody watching")}</span>
        </div>
      </div>
      ${this.view === "activity"
        ? html`<activity-view .status=${s} .channels=${this.channels} .reload=${() => this.refresh()}></activity-view>`
        : html`<channel-board .channels=${this.channels} .jellyfin=${s?.jellyfin_playing || []} .reload=${() => this.refresh()}></channel-board>`}
      <share-dialog .access=${this.access} heading="bifrost ${s?.name ?? ""}"></share-dialog>
      ${this.toasts.view(true)}
      <details class="note more"><summary>${info}<span>${t("How this page works")}</span></summary><p>${t("Nothing on this page opens a channel: states come from real opens since bifrost last started, and from what Jellyfin reports about its own sessions. A channel with several sources skips a dead one by itself. Refreshes every 3 s while in view.")}</p></details>
    </div>`;
  }
}
customElements.define("bifrost-app", BifrostApp);
