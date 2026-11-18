// kubeweave web UI. Plain JavaScript, no build step. Data comes from the kubeweave JSON API (/api/*).
"use strict";

const TOKEN = document.querySelector('meta[name="kubeweave-token"]')?.content || ""; // normally empty: an HttpOnly cookie authenticates
const $ = (s) => document.querySelector(s);

// h builds DOM nodes; text is always set with textContent, never parsed as HTML.
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "text") el.textContent = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}
const svg = (tag, attrs, ...kids) => {
  const el = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [k, v] of Object.entries(attrs || {})) if (v != null) el.setAttribute(k, v);
  for (const kid of kids.flat()) if (kid != null) el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  return el;
};

const HEADERS = { "X-Kubeweave": "1", "Content-Type": "application/json", ...(TOKEN ? { "X-Kubeweave-Token": TOKEN } : {}) };

async function api(path, params, opts) {
  const q = new URLSearchParams(params || {});
  const sep = path.includes("?") ? "&" : "?";
  const res = await fetch(`/api/${path}${sep}${q}`, { ...(opts || {}), headers: HEADERS });
  if (res.status === 403 && S.meta && S.meta.user) { location.href = "/auth/login?back=/"; return {}; }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}
const post = (path, body) => api(path, {}, { method: "POST", body: JSON.stringify(body) });

// streamText POSTs and calls onChunk with text as it arrives (AI answers).
async function streamText(path, body, onChunk, signal) {
  const res = await fetch(`/api/${path}`, { method: "POST", headers: HEADERS, body: JSON.stringify(body), signal });
  if (!res.ok) { const b = await res.json().catch(() => ({})); throw new Error(b.error || res.statusText); }
  const reader = res.body.getReader(), dec = new TextDecoder();
  for (;;) { const { done, value } = await reader.read(); if (done) break; onChunk(dec.decode(value, { stream: true })); }
}

function toast(msg, isErr) {
  const t = h("div", { class: "toast" + (isErr ? " err" : ""), role: isErr ? "alert" : "status", text: msg });
  $("#toasts").append(t);
  setTimeout(() => t.remove(), isErr ? 8000 : 3500);
}

const S = {
  meta: null, ctx: "", ns: "all", resources: [], res: "pods",
  table: { columns: [], rows: [], total: 0 }, filter: "", sort: { col: -1, dir: 1 }, view: [], cursor: 0, limit: 500,
  picked: new Set(), sel: null, tab: "describe", logSrc: null, timer: null, collapsed: {}, chat: [], aiAbort: null, graphMode: "diagram",
};

const HEALTH_DOT = { ok: "●", warn: "▲", err: "✖", neutral: "○" };
const HEALTH_WORD = { ok: "healthy", warn: "warning", err: "error", neutral: "" };
const TABS = [
  { id: "describe", label: "Describe", key: "d" }, { id: "yaml", label: "YAML", key: "y" },
  { id: "edit", label: "Edit", key: "e", needs: (r) => r.name !== "secrets" },
  { id: "timeline", label: "Timeline", key: "t", needs: (r) => r.graph }, { id: "graph", label: "Relations", key: "w", needs: (r) => r.graph },
  { id: "logs", label: "Logs", key: "l", needs: (r) => r.logs }, { id: "metrics", label: "Metrics", key: "m", needs: (r) => r.name === "pods" },
  { id: "ai", label: "AI", key: "a", needs: (r) => S.meta.ai && r.name !== "secrets" },
];

const resInfo = (name) => S.resources.find((r) => r.name === name) || { name, kind: name, namespaced: true };
const favorites = () => (S.meta.favorites && S.meta.favorites.length ? S.meta.favorites : ["pods", "deployments", "services", "events", "nodes"]);
const rowKey = (r) => (r.ns || "") + "/" + r.name;
const isDanger = () => S.meta.contexts.find((c) => c.name === S.ctx)?.danger;
const roMsg = "Read-only: start kubeweave serve with --allow-changes";

// ---------- URL (deep links) ----------

function writeHash() {
  const parts = [S.ctx, S.ns, S.res].map(encodeURIComponent);
  if (S.sel) parts.push(encodeURIComponent(S.sel.ns || "-"), encodeURIComponent(S.sel.name), S.tab);
  const h = "#/" + parts.join("/");
  if (location.hash !== h) history.replaceState(null, "", location.pathname + location.search + h);
}

function readHash() {
  const p = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  return { ctx: p[0], ns: p[1], res: p[2], selNS: p[3] === "-" ? "" : p[3], selName: p[4], tab: p[5] };
}

// ---------- boot ----------

async function boot() {
  try {
    S.meta = await api("meta");
  } catch (e) {
    $("#title").textContent = "Cannot reach kubeweave: " + e.message;
    return;
  }
  const link = readHash();
  S.ctx = link.ctx && S.meta.contexts.some((c) => c.name === link.ctx) ? link.ctx : S.meta.context;
  S.ns = link.ns || S.meta.namespace || "all";
  S.res = link.res || "pods";
  const mode = $("#mode");
  mode.textContent = S.meta.readOnly ? "READ-ONLY" : "CHANGES ALLOWED";
  mode.className = "badge " + (S.meta.readOnly ? "ro" : "rw");
  $("#termLink").href = "/term" + (TOKEN ? "?t=" + encodeURIComponent(TOKEN) : "");
  if (S.meta.user) $("#user").replaceChildren(h("span", { class: "muted", text: S.meta.user }), " ",
    h("form", { method: "post", action: "/auth/logout", class: "inline" }, h("button", { class: "ghost", type: "submit" }, "Sign out")));
  $("#chatBtn").classList.toggle("hidden", !S.meta.ai);
  wireChrome();
  await loadContext(false);
  if (isSpecial()) refreshView();
  else if (link.selName) openPanel({ ns: link.selNS, name: link.selName }, link.tab || "describe");
  refreshForwards();
}

async function loadContext(resetNS) {
  $("#ctx").replaceChildren(...S.meta.contexts.map((c) => h("option", { value: c.name, selected: c.name === S.ctx, text: (c.danger ? "⚠ " : "") + c.name })));
  document.querySelector(".top").classList.toggle("danger", !!isDanger());
  if (resetNS) S.ns = "all";
  const [res, ns] = await Promise.all([api("resources", { ctx: S.ctx }).catch(() => []), api("namespaces", { ctx: S.ctx }).catch((e) => ({ names: ["all"], note: e.message }))]);
  S.resources = res;
  const names = ns.names.includes(S.ns) ? ns.names : [...ns.names, S.ns];
  $("#ns").replaceChildren(...names.map((n) => h("option", { value: n, selected: n === S.ns, text: n })));
  if (ns.note && resetNS) toast(ns.note, true);
  renderSide();
  await loadList(true);
}

// ---------- sidebar ----------

function renderSide() {
  const favs = favorites().map((n, i) => sideLink(resInfo(n), i < 9 ? String(i + 1) : ""));
  const groups = {};
  for (const r of S.resources) (groups[r.group || "Other APIs"] ||= []).push(r);
  const order = [...(S.meta.groups || []), ...Object.keys(groups).filter((g) => !(S.meta.groups || []).includes(g))];
  const special = (id, label, fn, key) => h("a", { href: "#", class: (S.res === id ? "on " : "") + (id === "@health" ? "health" : ""), "aria-current": S.res === id ? "page" : null,
    onclick: (e) => { e.preventDefault(); fn(); } }, label, key ? h("kbd", { text: key }) : null);
  const parts = [special("@health", "⚕ Health check", showHealth, "H"), special("@changes", "↻ What changed?", showChanges, "C"),
    special("@slos", "◎ SLOs", showSLOs, ""), special("@snapshots", "▣ Snapshots", showSnapshots, ""),
    h("h3", { text: "Favourites" }), ...favs];
  for (const g of order) {
    if (!groups[g]) continue;
    const closed = S.collapsed[g] ?? (g === "Custom resources" || g === "Other APIs");
    parts.push(h("h3", { role: "button", tabindex: "0", "aria-expanded": String(!closed), onclick: () => { S.collapsed[g] = !closed; renderSide(); },
      onkeydown: (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); S.collapsed[g] = !closed; renderSide(); } } }, g, h("span", { "aria-hidden": "true", text: closed ? "+" : "−" })));
    if (!closed) parts.push(...groups[g].sort((a, b) => a.kind.localeCompare(b.kind)).map((r) => sideLink(r, "")));
  }
  $("#side").replaceChildren(...parts);
}

function sideLink(r, key) {
  return h("a", { href: "#", class: r.name === S.res ? "on" : "", "aria-current": r.name === S.res ? "page" : null, title: r.apiGroup ? r.name : r.kind,
    onclick: (e) => { e.preventDefault(); switchRes(r.name); } }, r.short && key ? r.short : r.kind, key ? h("kbd", { text: key }) : null);
}

async function switchRes(name) {
  if (S.res === name) return;
  S.res = name; S.filter = ""; $("#filter").value = ""; S.sort = { col: -1, dir: 1 }; S.cursor = 0; S.picked.clear(); S.limit = 500;
  closePanel();
  renderSide();
  await loadList(true);
}

// ---------- table (filtered, sorted and paged on the server) ----------

async function loadList(showLoading) {
  clearTimeout(S.timer);
  if (isSpecial()) return;
  const r = resInfo(S.res);
  $("#title").textContent = `${r.kind}${r.kind.endsWith("s") ? "" : "s"} · ${r.namespaced ? S.ns : "cluster"}`;
  if (showLoading) $("#note").textContent = "loading…";
  try {
    const p = { ctx: S.ctx, ns: S.ns, res: S.res, filter: S.filter, limit: String(S.limit) };
    if (S.sort.col >= 0) { p.sort = String(S.sort.col); p.dir = S.sort.dir > 0 ? "asc" : "desc"; }
    const t = await api("list", p);
    S.table = t;
    const shown = t.rows.length < t.total ? `showing ${t.rows.length} of ${t.total}` : `${t.total} items`;
    $("#note").textContent = (t.note ? t.note + " · " : "") + shown + (t.all !== t.total ? ` (${t.all} before filter)` : "");
    $("#updated").textContent = "updated " + new Date().toLocaleTimeString();
    renderTable();
  } catch (e) {
    S.table = { columns: [], rows: [], total: 0 };
    renderTable(e.message);
  }
  writeHash();
  S.timer = setTimeout(function again() { if (document.hidden) S.timer = setTimeout(again, 5000); else loadList(false); }, 5000);
}

function sparkline(vals) {
  if (!vals || vals.length < 2) return h("span", { class: "muted", text: "…" });
  const w = 70, ht = 16, mx = Math.max(...vals, 1);
  const pts = vals.map((v, i) => `${(i / (vals.length - 1)) * w},${ht - (v / mx) * (ht - 2) - 1}`).join(" ");
  return svg("svg", { width: w, height: ht, viewBox: `0 0 ${w} ${ht}`, class: "spark", role: "img", "aria-label": `CPU trend, latest ${vals[vals.length - 1]}m` },
    svg("polyline", { points: pts, fill: "none", stroke: "currentColor", "stroke-width": "1.5" }));
}

function renderTable(err) {
  S.view = S.table.rows;
  S.cursor = Math.min(S.cursor, Math.max(0, S.view.length - 1));
  const trend = S.res === "pods";
  const all = S.view.length > 0 && S.view.every((r) => S.picked.has(rowKey(r)));
  const pickable = !isSpecial();
  const head = h("tr", {}, !pickable ? h("th", { class: "pick" }) : h("th", { class: "pick" }, h("input", { type: "checkbox", "aria-label": "Select all rows", checked: all,
    onchange: (e) => { S.view.forEach((r) => (e.target.checked ? S.picked.add(rowKey(r)) : S.picked.delete(rowKey(r)))); renderTable(); } })),
    h("th", { class: "dot" }, h("span", { class: "sr", text: "Health" })),
    ...S.table.columns.map((c, i) => h("th", { class: S.sort.col === i ? "sorted" : "", scope: "col", tabindex: "0",
      "aria-sort": S.sort.col === i ? (S.sort.dir > 0 ? "ascending" : "descending") : "none",
      onclick: () => sortBy(i), onkeydown: (e) => { if (e.key === "Enter") sortBy(i); } }, c, S.sort.col === i ? (S.sort.dir > 0 ? " ▲" : " ▼") : "")),
    trend ? h("th", { text: "CPU TREND" }) : null);
  $("#table thead").replaceChildren(head);
  const selKey = S.sel ? S.sel.ns + "/" + S.sel.name : null;
  $("#table tbody").replaceChildren(...S.view.map((r, i) => h("tr", {
    class: (i === S.cursor ? "cur " : "") + (S.picked.has(rowKey(r)) ? "picked" : ""), "aria-selected": String(i === S.cursor),
    onclick: (e) => { if (e.target.tagName === "INPUT") return; S.cursor = i; markCursor(); if (S.sel) openPanel(r, S.tab); },
    ondblclick: () => openPanel(r, S.tab),
  }, !pickable ? h("td", { class: "pick" }) : h("td", { class: "pick" }, h("input", { type: "checkbox", "aria-label": "Select " + r.name, checked: S.picked.has(rowKey(r)),
    onchange: (e) => { e.target.checked ? S.picked.add(rowKey(r)) : S.picked.delete(rowKey(r)); renderBulk(); } })),
    h("td", { class: "dot h-" + r.health, title: HEALTH_WORD[r.health] }, h("span", { "aria-hidden": "true", text: HEALTH_DOT[r.health] || "" }), h("span", { class: "sr", text: HEALTH_WORD[r.health] })),
    ...r.cells.map((c) => h("td", { text: c, title: c.length > 40 ? c : null })), trend ? h("td", { class: "h-" + r.health }, sparkline(r.trend)) : null)));
  const empty = $("#empty");
  empty.classList.toggle("hidden", S.view.length > 0);
  empty.textContent = err ? "✗ " + err : S.filter ? "No rows match the filter." : "Nothing here.";
  $("#more").classList.toggle("hidden", !(S.table.rows.length < S.table.total));
  if (selKey) { const i = S.view.findIndex((r) => rowKey(r) === selKey); if (i >= 0) { S.cursor = i; markCursor(); } }
  renderBulk();
}

function sortBy(i) { S.sort = S.sort.col === i ? { col: i, dir: -S.sort.dir } : { col: i, dir: 1 }; loadList(false); }

function markCursor() {
  document.querySelectorAll("#table tbody tr").forEach((tr, i) => { tr.classList.toggle("cur", i === S.cursor); tr.setAttribute("aria-selected", String(i === S.cursor)); });
  document.querySelector("#table tbody tr.cur")?.scrollIntoView({ block: "nearest" });
}

const current = () => S.view[S.cursor];

// ---------- multi-select ----------

function pickedRows() { return S.table.rows.filter((r) => S.picked.has(rowKey(r))); }

let bulkSig = "";
function renderBulk() {
  const rows = pickedRows(), bar = $("#bulk");
  bar.classList.toggle("hidden", rows.length === 0);
  const sig = S.res + "|" + rows.map(rowKey).join(",");
  if (!rows.length || sig === bulkSig) { if (!rows.length) bulkSig = ""; return; } // keep buttons stable across refreshes
  bulkSig = sig;
  const r = resInfo(S.res), ro = S.meta.readOnly;
  bar.replaceChildren(h("b", { text: `${rows.length} selected` }),
    r.restart ? h("button", { class: "ghost", disabled: ro, title: ro ? roMsg : null, onclick: () => bulk("restart", rows) }, "Restart all") : null,
    h("button", { class: "ghost", disabled: ro, title: ro ? roMsg : null, onclick: () => bulk("delete", rows) }, "Delete all"),
    rows.length === 2 ? h("button", { class: "ghost", onclick: () => compare(rows[0], rows[1]) }, "Compare") : null,
    h("button", { class: "ghost", onclick: () => { S.picked.clear(); renderTable(); } }, "Clear"));
}

function bulk(action, rows) {
  if (S.meta.readOnly) { toast(roMsg, true); return; }
  const word = String(rows.length);
  const typed = h("input", { type: "text", "aria-label": "Type the number of objects", placeholder: word });
  dialog(`${action === "delete" ? "Delete" : "Restart"} ${rows.length} ${resInfo(S.res).kind}s on ${S.ctx}?`,
    [h("ul", {}, ...rows.slice(0, 15).map((r) => h("li", { text: rowKey(r) }))), rows.length > 15 ? h("p", { class: "muted", text: `…and ${rows.length - 15} more` }) : null,
      h("p", { text: `Type ${word} to confirm.` }), typed],
    (close) => [h("button", { class: "ghost", onclick: close }, "Cancel"), h("button", { class: "btn danger", onclick: async () => {
      if (typed.value.trim() !== word) { toast("Type the number of objects", true); return; }
      close();
      let ok = 0;
      for (const r of rows) {
        try { await post("action", { Ctx: S.ctx, NS: r.ns, Res: S.res, Name: r.name, Action: action, Confirm: r.name }); ok++; }
        catch (e) { toast(`${r.name}: ${e.message}`, true); }
      }
      toast(`✓ ${action} done for ${ok} of ${rows.length}`);
      S.picked.clear(); loadList(false);
    } }, "Confirm")]);
}

async function compare(a, b) {
  try {
    const d = await api("compare", { ctx: S.ctx, res: S.res, ns: a.ns || "", name: a.name, ns2: b.ns || "", name2: b.name });
    dialog(`${a.name} ⇄ ${b.name}`, diffView(d.diff, "No differences."), (close) => [h("button", { class: "btn", onclick: close }, "Close")], "wide");
  } catch (e) { toast(e.message, true); }
}

function diffView(lines, emptyText) {
  if (!lines || !lines.some((l) => l.startsWith("+") || l.startsWith("-"))) return h("p", { class: "muted", text: emptyText || "No changes." });
  return h("pre", { class: "code diff" }, ...lines.map((l) => l === "@@" ? h("div", { class: "muted", text: "  ⋯" })
    : h("div", { class: l[0] === "+" ? "add" : l[0] === "-" ? "del" : "", text: (l[0] === " " ? "  " : l[0] + " ") + l.slice(1) })));
}

// ---------- health ----------

async function showHealth() {
  clearTimeout(S.timer);
  S.res = "@health"; S.picked.clear(); closePanel(); renderSide();
  $("#title").textContent = "Health check · " + S.ns;
  $("#note").textContent = "scanning…";
  try {
    const is = await api("issues", { ctx: S.ctx, ns: S.ns });
    S.table = { columns: ["SEVERITY", "KIND", "NAMESPACE", "NAME", "PROBLEM", "WHAT TO DO"], total: is.length,
      rows: is.map((i) => ({ ns: i.NS, name: i.Name, kind: i.Kind, health: i.Severity, cells: [i.Severity === "err" ? "error" : "warning", i.Kind, i.NS, i.Name, i.Reason + ": " + i.Message, i.Hint] })) };
    $("#note").textContent = is.length ? `${is.length} issues` : "✓ no issues found";
  } catch (e) { S.table = { columns: [], rows: [], total: 0 }; renderTable(e.message); return; }
  renderTable();
}

// ---------- what changed, SLOs, snapshots ----------

const isSpecial = () => S.res.startsWith("@");

async function special(id, title, load) {
  clearTimeout(S.timer);
  S.res = id; S.picked.clear(); closePanel(); renderSide();
  $("#title").textContent = title + " · " + S.ns;
  $("#note").textContent = "loading…";
  try { S.table = await load(); $("#note").textContent = S.table.note || `${S.table.total} items`; renderTable(); }
  catch (e) { S.table = { columns: [], rows: [], total: 0 }; renderTable(e.message); }
  writeHash();
}

async function showChanges() {
  S.since = S.since || "2h";
  await special("@changes", "What changed", async () => {
    const cs = await api("changes", { ctx: S.ctx, ns: S.ns, since: S.since });
    return { columns: ["WHEN", "SOURCE", "KIND", "NAMESPACE", "NAME", "WHAT"], total: cs.length, note: cs.length ? `${cs.length} changes · rollouts, config, scaling, Helm, GitOps` : `nothing changed in the last ${S.since}`,
      rows: cs.map((c) => ({ ns: c.NS, name: c.Name, kind: c.Kind, health: ["rollout", "helm", "gitops"].includes(c.Source) ? "warn" : "neutral",
        cells: [new Date(c.Time).toLocaleString(), c.Source, c.Kind, c.NS, c.Name, c.What] })) };
  });
  const pick = h("select", { "aria-label": "Time window", onchange: (e) => { S.since = e.target.value; showChanges(); } },
    ...["30m", "2h", "6h", "24h", "7d"].map((v) => h("option", { value: v, selected: v === S.since, text: "last " + v })));
  $("#note").prepend(pick, " ");
}

function showSLOs() {
  return special("@slos", "SLOs", async () => {
    const ss = await api("slos", { ctx: S.ctx });
    const pct = (v) => (v * 100).toFixed(3) + "%";
    return { columns: ["SLO", "OBJECTIVE", "WINDOW", "AVAILABILITY", "BUDGET LEFT", "BURN (1h)", "STATUS"], total: ss.length,
      note: ss.length ? "error budgets from Prometheus" : "no SLOs for this cluster · add slos: to the kubeweave config",
      rows: ss.map((x) => {
        const st = x.Error ? ["err", "✗ " + x.Error] : x.Budget < 0 ? ["err", "budget spent"] : x.Burn1 > 14.4 ? ["err", "burning fast"] : x.Burn1 > 1 || x.Budget < 0.25 ? ["warn", "at risk"] : ["ok", "within budget"];
        return { ns: "", name: x.Name, health: st[0], cells: [x.Name, pct(x.Objective), x.Window, pct(x.Availability), (x.Budget * 100).toFixed(0) + "%", x.Burn1.toFixed(1) + "x", st[1]] };
      }) };
  });
}

function showSnapshots() {
  if (S.ns === "all") { toast("Pick a namespace first: snapshots are per namespace", true); return; }
  return special("@snapshots", "Snapshots", async () => {
    const ss = await api("snapshots", { ctx: S.ctx, ns: S.ns });
    return { columns: ["NAME", "TAKEN", "OBJECTS"], total: ss.length, note: "double-click compares with now · S saves a new one",
      rows: ss.map((x) => ({ ns: S.ns, name: x.Name, health: "neutral", cells: [x.Name, new Date(x.Taken).toLocaleString(), x.Objects] })) };
  });
}

async function saveSnapshot() {
  try { const r = await post("snapshots", { Ctx: S.ctx, NS: S.ns, Name: "" }); toast("✓ " + r.status); showSnapshots(); } catch (e) { toast(e.message, true); }
}

async function snapshotDiff(name) {
  try {
    const d = await api("snapshot/diff", { ctx: S.ctx, ns: S.ns, name });
    const parts = d.changes.length ? d.changes.map((c) => h("details", { open: c.State !== "changed" ? null : true },
      h("summary", { class: c.State === "added" ? "h-ok" : c.State === "removed" ? "h-err" : "h-warn", text: `${c.State} ${c.Key}` }), c.State === "changed" ? diffView(c.Diff) : null))
      : [h("p", { class: "h-ok", text: "✓ nothing changed since " + new Date(d.taken).toLocaleString() })];
    dialog(`Snapshot ${name} → now`, parts, (close) => [h("button", { class: "btn", onclick: close }, "Close")], "wide");
  } catch (e) { toast(e.message, true); }
}

// ---------- detail panel ----------

function openPanel(row, tab) {
  if (S.res === "@snapshots") { snapshotDiff(row.name); return; }
  if (S.res === "@slos") return;
  const kindRes = (k) => KIND_RES[k] || (S.resources.find((r) => r.kind === k || r.name === k) || {}).name || k;
  const r = resInfo(isSpecial() ? kindRes(row.kind) : S.res);
  S.sel = { ns: row.ns, name: row.name, res: r.name };
  const tabs = TABS.filter((t) => !t.needs || t.needs(r));
  if (!tabs.some((t) => t.id === tab)) tab = "describe";
  $("#panel").classList.remove("hidden");
  $("#pkind").textContent = r.kind + (row.ns ? " · " + row.ns : "");
  $("#pname").textContent = row.name;
  const sel = S.sel;
  api("owner", { ctx: S.ctx, ns: row.ns || "", res: r.name, name: row.name }).then((o) => {
    if (S.sel !== sel || !o.found) return;
    $("#pkind").append(h("span", { class: "owner", title: "from " + o.source }, " · owned by ", h("b", { text: o.team }), o.contact ? " (" + o.contact + ")" : ""));
  }).catch(() => {});
  $("#ptabs").replaceChildren(...tabs.map((t) => h("button", { class: t.id === tab ? "on" : "", role: "tab", id: "tab-" + t.id, "aria-selected": String(t.id === tab),
    "aria-controls": "pbody", onclick: () => showTab(t.id) }, t.label, h("kbd", { "aria-hidden": "true", text: t.key }))));
  const ro = S.meta.readOnly, why = ro ? roMsg : null;
  $("#pactions").replaceChildren(...[
    r.scale ? h("button", { class: "ghost", disabled: ro, title: why || "Scale (S)", onclick: () => actScale() }, "Scale") : null,
    r.restart ? h("button", { class: "ghost", disabled: ro, title: why || "Rollout restart (R)", onclick: () => actRestart() }, "Restart") : null,
    (r.name === "pods" || r.name === "services" || r.logs) ? h("button", { class: "ghost", disabled: ro, title: why || "Port-forward and open in a new tab (F)", onclick: () => actForward() }, "Forward") : null,
    h("button", { class: "ghost", disabled: ro, title: why || "Delete (Ctrl+D)", onclick: () => actDelete() }, "Delete"),
    h("button", { class: "ghost", title: "Copy a link to this view", onclick: () => { navigator.clipboard?.writeText(location.href.replace(/\?t=[^#]*/, "")); toast("Link copied (the session token is not included)"); } }, "Link")].filter(Boolean));
  showTab(tab);
}

function closePanel() {
  stopLogs();
  if (S.aiAbort) S.aiAbort.abort();
  S.sel = null;
  $("#panel").classList.add("hidden");
  writeHash();
}

const target = () => ({ ctx: S.ctx, ns: S.sel.ns || "", res: S.sel.res, name: S.sel.name });
const targetBody = () => ({ Ctx: S.ctx, NS: S.sel.ns || "", Res: S.sel.res, Name: S.sel.name });

async function showTab(tab) {
  stopLogs();
  if (S.aiAbort) { S.aiAbort.abort(); S.aiAbort = null; }
  S.tab = tab;
  document.querySelectorAll("#ptabs button").forEach((b) => { const on = b.id === "tab-" + tab; b.classList.toggle("on", on); b.setAttribute("aria-selected", String(on)); });
  $("#pbody").setAttribute("aria-labelledby", "tab-" + tab);
  writeHash();
  const body = $("#pbody");
  body.replaceChildren(h("div", { class: "muted", text: "loading…" }));
  const sel = S.sel, still = () => S.sel === sel && S.tab === tab;
  try {
    if (tab === "describe" || tab === "yaml") {
      const out = await api(tab, target());
      if (!still()) return;
      body.replaceChildren(h("div", { class: "toolbar" }, h("button", { class: "ghost", onclick: () => { navigator.clipboard?.writeText(out.text); toast("Copied"); } }, "Copy"),
        tab === "yaml" ? h("span", { class: "muted", text: "Secret values are masked." }) : null), codeBlock(out.text, tab === "yaml"));
    } else if (tab === "edit") await showEdit(body, still);
    else if (tab === "timeline") {
      const tl = await api("timeline", target());
      if (!still()) return;
      body.replaceChildren(tl.length ? h("ul", { class: "tl" }, ...tl.map((e) => h("li", {},
        h("span", { class: "muted", text: new Date(e.Time).toLocaleString() }), h("span", { class: "src h-" + e.Health, text: e.Source }),
        h("span", {}, h("b", { text: e.Object + " " }), e.Text, e.Count > 1 ? h("span", { class: "muted", text: ` ×${e.Count}` }) : null)))) : h("div", { class: "muted", text: "Nothing in the timeline." }));
    } else if (tab === "graph") {
      const g = await api("graph", target());
      if (!still()) return;
      renderGraph(body, g);
    } else if (tab === "logs") await showLogs(body);
    else if (tab === "metrics") await showMetrics(body, still);
    else if (tab === "ai") showAI(body);
  } catch (e) {
    if (still()) body.replaceChildren(h("div", { class: "h-err", role: "alert", text: "✗ " + e.message }));
  }
}

function codeBlock(text, yaml) {
  const pre = h("pre", { class: "code" });
  for (const line of text.split("\n")) {
    const m = yaml ? /^(\s*-?\s*)([\w.\/-]+:)(.*)$/.exec(line) : /^(\s*)([A-Z][\w ]+:)(.*)$/.exec(line);
    if (m) pre.append(m[1], h("span", { class: "k", text: m[2] }), m[3], "\n");
    else pre.append(line, "\n");
  }
  return pre;
}

// ---------- edit (dry-run, diff, apply) ----------

async function showEdit(body, still, preset) {
  const out = preset ? { yaml: preset.yaml } : await api("edit", target());
  if (!still()) return;
  const ta = h("textarea", { class: "editor", spellcheck: "false", "aria-label": "YAML to apply" });
  ta.value = out.yaml;
  const result = h("div", { class: "result", "aria-live": "polite" });
  const apply = h("button", { class: "btn danger", disabled: true, title: S.meta.readOnly ? roMsg : "Run the dry-run first" }, "Apply");
  let checked = "";
  const check = async () => {
    result.replaceChildren(h("div", { class: "muted", text: "⋯ server-side dry-run…" }));
    try {
      const d = await post("dryrun", { ...targetBody(), YAML: ta.value });
      checked = d.ok ? ta.value : "";
      result.replaceChildren(d.ok ? h("div", { class: "h-ok", text: "✓ The API server accepts this change (validation, admission webhooks, policies)." })
        : h("div", { class: "h-err", text: "✗ The API server would reject this: " + d.error }),
        ...(d.warnings || []).map((w) => h("div", { class: "h-warn", text: "▲ " + w })), diffView(d.diff, "No changes yet."));
      apply.disabled = !d.ok || S.meta.readOnly || !(d.diff || []).some((l) => l[0] === "+" || l[0] === "-");
    } catch (e) { result.replaceChildren(h("div", { class: "h-err", text: "✗ " + e.message })); apply.disabled = true; }
  };
  ta.addEventListener("input", () => { apply.disabled = true; });
  ta.addEventListener("keydown", (e) => { if ((e.ctrlKey || e.metaKey) && e.key === "Enter") { e.preventDefault(); check(); } });
  apply.onclick = () => {
    if (ta.value !== checked) { toast("Run the dry-run again: the YAML changed", true); return; }
    confirmAction(`Apply this change to ${S.sel.name} on ${S.ctx}?`, "Replaces the object with the YAML above (it passed the dry-run).",
      async (c) => { const r = await post("apply", { ...targetBody(), YAML: ta.value, Confirm: c }); showTab("yaml"); return r; });
  };
  body.replaceChildren(h("div", { class: "toolbar" }, h("button", { class: "btn", onclick: check }, "Dry-run & diff"), apply,
    h("span", { class: "muted", text: preset ? "AI-proposed change · review before applying" : "Ctrl+Enter runs the dry-run" })),
    preset && preset.why ? h("div", { class: "why" }, ...preset.why.split("\n").map((l) => h("div", { text: l }))) : null, ta, result);
  if (preset) check(); else ta.focus();
}

// ---------- relations: interactive diagram ----------

const KIND_RES = { Pod: "pods", Deployment: "deployments", StatefulSet: "statefulsets", DaemonSet: "daemonsets", ReplicaSet: "replicasets",
  Service: "services", Ingress: "ingresses", ConfigMap: "configmaps", Secret: "secrets", PersistentVolumeClaim: "persistentvolumeclaims", HPA: "horizontalpodautoscalers" };

async function openNode(n) {
  const res = KIND_RES[n.kind];
  if (!res) return;
  if (S.res !== res) { S.res = res; renderSide(); await loadList(true); }
  openPanel({ ns: n.ns, name: n.name.split(" ")[0] }, "describe");
}

function renderGraph(body, g) {
  const toggle = h("div", { class: "toolbar" }, ...["diagram", "list"].map((m) => h("button", { class: "ghost" + (S.graphMode === m ? " on" : ""), "aria-pressed": String(S.graphMode === m),
    onclick: () => { S.graphMode = m; renderGraph(body, g); } }, m === "diagram" ? "Diagram" : "List")), h("span", { class: "muted", text: "Click a part to open it · drag to pan · wheel to zoom" }));
  body.replaceChildren(toggle, S.graphMode === "list" ? graphList(g) : graphDiagram(g));
}

function graphList(g) {
  const node = (n) => !n ? null : h("li", {}, h("span", { class: "node", tabindex: KIND_RES[n.kind] ? "0" : null, onclick: () => openNode(n), onkeydown: (e) => { if (e.key === "Enter") openNode(n); } },
    h("span", { class: "h-" + n.health, text: (HEALTH_DOT[n.health] || "") + " " }), h("span", { class: "kind", text: n.kind }), n.name,
    n.status ? h("span", { class: "muted", text: "  " + n.status }) : null, n.note ? h("span", { class: "h-warn", text: "  " + n.note }) : null),
    n.children && n.children.length ? h("ul", {}, ...n.children.map(node)) : null);
  const parts = [];
  if (g.front?.children?.length) parts.push(h("div", { class: "muted", text: "Reached through" }), h("ul", { class: "tree" }, ...g.front.children.map(node)));
  parts.push(h("div", { class: "muted", text: "Made of" }), h("ul", { class: "tree" }, node(g.root)));
  return h("div", {}, ...parts);
}

// graphDiagram lays the relations out left to right: what reaches the object, the object, what it is made of.
function graphDiagram(g) {
  const W = 200, H = 40, GX = 60, GY = 14;
  const nodes = [], edges = [];
  let row = 0;
  const place = (n, depth) => {
    const kids = (n.children || []).map((c) => place(c, depth + 1));
    const y = kids.length ? (kids[0].y + kids[kids.length - 1].y) / 2 : (row++) * (H + GY);
    const me = { n, x: depth * (W + GX), y };
    nodes.push(me);
    kids.forEach((k) => edges.push([me, k]));
    return me;
  };
  const front = g.front?.children || [];
  const root = place(g.root, front.length ? 1 : 0);
  front.forEach((f, i) => {
    const me = { n: f, x: 0, y: root.y + (i - (front.length - 1) / 2) * (H + GY) };
    nodes.push(me); edges.push([me, root]);
  });
  const minY = Math.min(...nodes.map((n) => n.y)), maxY = Math.max(...nodes.map((n) => n.y));
  const maxX = Math.max(...nodes.map((n) => n.x));
  const vbW = maxX + W + 20, vbH = maxY - minY + H + 20;
  const colour = { ok: "var(--ok)", warn: "var(--warn)", err: "var(--err)", neutral: "var(--muted)" };
  const layer = svg("g", { transform: `translate(10 ${10 - minY})` },
    ...edges.map(([a, b]) => svg("path", { class: "edge", d: `M${a.x + W},${a.y + H / 2} C${a.x + W + GX / 2},${a.y + H / 2} ${b.x - GX / 2},${b.y + H / 2} ${b.x},${b.y + H / 2}` })),
    ...nodes.map(({ n, x, y }, i) => {
      const clickable = !!KIND_RES[n.kind];
      const label = n.name.length > 26 ? n.name.slice(0, 25) + "…" : n.name;
      return svg("g", { class: "gnode" + (clickable ? " link" : ""), "data-i": i, transform: `translate(${x} ${y})`, tabindex: clickable ? "0" : null, role: clickable ? "button" : null,
        "aria-label": `${n.kind} ${n.name} ${HEALTH_WORD[n.health] || ""} ${n.status || ""}` },
        svg("title", {}, `${n.kind} ${n.name}${n.status ? " · " + n.status : ""}${n.note ? " · " + n.note : ""}`),
        svg("rect", { width: W, height: H, rx: 7, stroke: colour[n.health] || "var(--line)" }),
        svg("circle", { cx: 12, cy: H / 2, r: 4, fill: colour[n.health] || "var(--muted)" }),
        svg("text", { x: 22, y: 15, class: "gkind" }, n.kind), svg("text", { x: 22, y: 31, class: "gname" }, label));
    }));
  const el = svg("svg", { class: "graph", viewBox: `0 0 ${vbW} ${vbH}`, role: "group", "aria-label": "Relationship diagram" }, layer);
  let vb = { x: 0, y: 0, w: vbW, h: vbH }, drag = null;
  const setVB = () => el.setAttribute("viewBox", `${vb.x} ${vb.y} ${vb.w} ${vb.h}`);
  const nodeAt = (t) => { const g = t.closest?.(".gnode.link"); return g ? nodes[+g.dataset.i].n : null; };
  el.addEventListener("wheel", (e) => { e.preventDefault(); const f = e.deltaY > 0 ? 1.1 : 0.9; vb = { x: vb.x + vb.w * (1 - f) / 2, y: vb.y + vb.h * (1 - f) / 2, w: vb.w * f, h: vb.h * f }; setVB(); }, { passive: false });
  el.addEventListener("pointerdown", (e) => { drag = { x: e.clientX, y: e.clientY, vb: { ...vb }, moved: false }; });
  el.addEventListener("pointermove", (e) => { if (!drag) return; const s = vb.w / el.clientWidth; const dx = (e.clientX - drag.x) * s, dy = (e.clientY - drag.y) * s;
    if (Math.abs(dx) + Math.abs(dy) > 2) drag.moved = true; vb = { ...drag.vb, x: drag.vb.x - dx, y: drag.vb.y - dy }; setVB(); });
  el.addEventListener("pointerup", (e) => { const d = drag; drag = null; if (d && !d.moved) { const n = nodeAt(e.target); if (n) openNode(n); } });
  el.addEventListener("keydown", (e) => { if (e.key === "Enter") { const n = nodeAt(e.target); if (n) openNode(n); } });
  return el;
}

// ---------- logs ----------

async function showLogs(body) {
  const sel = S.sel;
  const pods = await api("pods", target());
  if (S.sel !== sel) return;
  if (!pods.length) { body.replaceChildren(h("div", { class: "muted", text: "No pods." })); return; }
  const podSel = h("select", { "aria-label": "Pod" }, ...pods.map((p) => h("option", { value: p.name, text: p.name })));
  const ctrSel = h("select", { "aria-label": "Container" });
  const fillCtr = () => { const p = pods.find((x) => x.name === podSel.value); ctrSel.replaceChildren(...p.containers.map((c) => h("option", { value: c, text: c }))); };
  fillCtr();
  const search = h("input", { type: "search", placeholder: "highlight…", "aria-label": "Highlight text in logs" });
  const follow = h("input", { type: "checkbox", checked: true });
  const prev = h("input", { type: "checkbox" });
  const out = h("div", { class: "logs", role: "log", "aria-live": "off" });
  const status = h("span", { class: "muted", role: "status" });
  const start = () => {
    stopLogs();
    out.replaceChildren();
    const q = new URLSearchParams({ ctx: S.ctx, ns: sel.ns, pod: podSel.value, container: ctrSel.value, tail: "500", previous: prev.checked ? "1" : "0" });
    if (TOKEN) q.set("t", TOKEN);
    const es = new EventSource("/api/logs?" + q);
    S.logSrc = es;
    status.textContent = "● streaming";
    let n = 0;
    es.onmessage = (e) => {
      const line = JSON.parse(e.data);
      const cls = /\b(error|fatal|panic|exception)\b/i.test(line) ? "err" : /\bwarn(ing)?\b/i.test(line) ? "warn" : "";
      const div = h("div", { class: cls });
      const term = search.value.trim();
      const i = term ? line.toLowerCase().indexOf(term.toLowerCase()) : -1;
      if (i >= 0) div.append(line.slice(0, i), h("mark", { text: line.slice(i, i + term.length) }), line.slice(i + term.length));
      else div.textContent = line;
      out.append(div);
      if (++n > 5000) out.firstChild.remove();
      if (follow.checked) body.scrollTop = body.scrollHeight;
    };
    es.addEventListener("end", () => { status.textContent = "stream ended"; es.close(); });
    es.onerror = () => { status.textContent = "disconnected"; es.close(); };
  };
  podSel.onchange = () => { fillCtr(); start(); };
  ctrSel.onchange = start;
  prev.onchange = start;
  const win = h("select", { "aria-label": "Download window" }, ...[["", "everything"], ["1h", "last 1h"], ["6h", "last 6h"], ["24h", "last 24h"]].map(([v, l]) => h("option", { value: v, text: l })));
  const dl = (path, params) => {
    const q = new URLSearchParams({ ctx: S.ctx, ns: sel.ns, ...params, since: win.value });
    if (TOKEN) q.set("t", TOKEN);
    const a = h("a", { href: `/api/${path}?${q}`, download: "" });
    document.body.append(a); a.click(); a.remove();
    toast("Download started");
  };
  body.replaceChildren(h("div", { class: "toolbar stack" },
    h("div", { class: "tbrow" }, podSel, ctrSel, search, h("label", {}, follow, " follow"), h("label", {}, prev, " previous"),
      h("button", { class: "ghost", onclick: () => { navigator.clipboard?.writeText(out.innerText); toast("Copied"); } }, "Copy"), status),
    h("div", { class: "tbrow" }, h("span", { class: "muted", text: "⬇ Download" }), win,
      h("button", { class: "ghost", title: "The complete log of this container, not just what is shown", onclick: () => dl("logs/download", { pod: podSel.value, container: ctrSel.value, previous: prev.checked ? "1" : "0", timestamps: "1" }) }, "This container (.log)"),
      h("button", { class: "ghost", title: "Every pod and container, including logs from before restarts, plus a summary", onclick: () => dl("logs/bundle", { res: sel.res, name: sel.name }) }, pods.length > 1 ? `All ${pods.length} pods (.zip)` : "All containers (.zip)"))),
    out);
  start();
}

function stopLogs() { if (S.logSrc) { S.logSrc.close(); S.logSrc = null; } }

// ---------- metrics ----------

function chart(points, key, label, fmt) {
  const W = 560, H = 140, P = 40;
  if (points.length < 2) return h("p", { class: "muted", text: `${label}: not enough samples yet (kubeweave samples while this page is open; configure Prometheus for history).` });
  const vals = points.map((p) => p[key]), mx = Math.max(...vals, 1), t0 = points[0].t, t1 = points[points.length - 1].t || t0 + 1;
  const x = (t) => P + ((t - t0) / Math.max(1, t1 - t0)) * (W - P - 6), y = (v) => H - 18 - (v / mx) * (H - 30);
  const line = points.map((p, i) => `${i ? "L" : "M"}${x(p.t)},${y(p[key])}`).join(" ");
  const last = vals[vals.length - 1];
  return h("figure", { class: "chart" }, h("figcaption", {}, h("b", { text: label }), h("span", { class: "muted", text: ` now ${fmt(last)} · peak ${fmt(mx)}` })),
    svg("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": `${label} over time, now ${fmt(last)}, peak ${fmt(mx)}` },
      svg("line", { x1: P, y1: H - 18, x2: W, y2: H - 18, class: "axis" }),
      svg("text", { x: 0, y: y(mx) + 4, class: "tick" }, fmt(mx)), svg("text", { x: 0, y: H - 14, class: "tick" }, "0"),
      svg("text", { x: P, y: H - 2, class: "tick" }, new Date(t0 * 1000).toLocaleTimeString()), svg("text", { x: W - 70, y: H - 2, class: "tick" }, new Date(t1 * 1000).toLocaleTimeString()),
      svg("path", { d: line, class: "series" })));
}

async function showMetrics(body, still) {
  const m = await api("metrics", { ctx: S.ctx, ns: S.sel.ns, pod: S.sel.name });
  if (!still()) return;
  const s = m.samples || [];
  const mib = (b) => (b / 1048576).toFixed(0) + "Mi";
  body.replaceChildren(h("p", { class: "muted", text: "Source: " + m.source }), chart(s, "cpu", "CPU", (v) => v + "m"), chart(s, "mem", "Memory", mib));
}

// ---------- AI ----------

function showAI(body) {
  const out = h("div", { class: "ai", "aria-live": "polite" });
  const run = async (label, path, payload) => {
    if (S.aiAbort) S.aiAbort.abort();
    S.aiAbort = new AbortController();
    out.replaceChildren(h("div", { class: "muted", text: label + "…" }));
    const text = h("div", { class: "aitext" });
    let started = false;
    try {
      await streamText(path, payload, (chunk) => { if (!started) { out.replaceChildren(text); started = true; } text.textContent += chunk; }, S.aiAbort.signal);
    } catch (e) { if (e.name !== "AbortError") out.replaceChildren(h("div", { class: "h-err", text: "✗ " + e.message })); }
  };
  const fix = async () => {
    out.replaceChildren(h("div", { class: "muted", text: "AI is preparing the smallest fix… (nothing changes until you apply)" }));
    try {
      const f = await post("ai/fix", targetBody());
      if (f.noChange) { out.replaceChildren(h("div", { class: "h-ok", text: "AI: no change needed." }), h("div", { class: "aitext", text: f.why })); return; }
      const tab = $("#tab-edit");
      if (tab) { S.tab = "edit"; document.querySelectorAll("#ptabs button").forEach((b) => { b.classList.toggle("on", b === tab); b.setAttribute("aria-selected", String(b === tab)); }); }
      await showEdit(body, () => true, { yaml: f.yaml, why: f.why });
    } catch (e) { out.replaceChildren(h("div", { class: "h-err", text: "✗ " + e.message })); }
  };
  body.replaceChildren(h("div", { class: "toolbar" },
    h("button", { class: "btn", onclick: () => run("Analyzing (manifest, events, logs; secrets redacted)", "ai/analyze", targetBody()) }, "Explain what is wrong"),
    h("button", { class: "ghost", onclick: fix }, "Propose a fix"),
    h("span", { class: "muted", text: "Secret values are never sent." })), out);
}

function openChat() {
  const log = h("div", { class: "chatlog", "aria-live": "polite" });
  const input = h("textarea", { rows: "2", placeholder: "Ask about this namespace… (Enter sends, Shift+Enter new line)", "aria-label": "Question for the AI" });
  const render = () => log.replaceChildren(...S.chat.map((m) => h("div", { class: "msg " + m.role }, h("b", { text: m.role === "user" ? "You" : "AI" }), h("div", { class: "aitext", text: m.content }))));
  const send = async () => {
    const q = input.value.trim();
    if (!q) return;
    input.value = "";
    S.chat.push({ role: "user", content: q }, { role: "assistant", content: "" });
    render();
    const msgs = S.chat.slice(0, -1).map((m) => ({ Role: m.role, Content: m.content }));
    try {
      await streamText("ai/chat", { Ctx: S.ctx, NS: S.ns, Messages: msgs }, (c) => { S.chat[S.chat.length - 1].content += c; render(); log.scrollTop = log.scrollHeight; });
    } catch (e) { S.chat[S.chat.length - 1].content = "✗ " + e.message; render(); }
  };
  input.addEventListener("keydown", (e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); e.stopPropagation(); send(); } });
  render();
  dialog(`AI · ${S.ctx} · ${S.ns}`, [log, input], (close) => [h("button", { class: "ghost", onclick: () => { S.chat = []; render(); } }, "New chat"), h("button", { class: "btn", onclick: send }, "Send")], "wide");
  input.focus();
}

// ---------- dialogs & actions ----------

let lastFocus = null;
function dialog(title, content, buttons, size) {
  const ov = $("#overlay");
  lastFocus = document.activeElement;
  const close = () => { ov.classList.add("hidden"); ov.replaceChildren(); lastFocus?.focus?.(); };
  const titleId = "dlg-" + Math.random().toString(36).slice(2);
  const d = h("div", { class: "dialog" + (size ? " " + size : ""), role: "dialog", "aria-modal": "true", "aria-labelledby": titleId },
    h("h3", { id: titleId, text: title }), h("div", { class: "dbody" }, content), buttons ? h("div", { class: "dfoot" }, ...buttons(close)) : null);
  ov.classList.remove("hidden");
  ov.replaceChildren(d);
  ov.onclick = (e) => { if (e.target === ov) close(); };
  d.addEventListener("keydown", (e) => { // keep Tab inside the dialog
    if (e.key !== "Tab") return;
    const f = [...d.querySelectorAll("button, input, select, textarea, a[href]")].filter((x) => !x.disabled);
    if (!f.length) return;
    if (e.shiftKey && document.activeElement === f[0]) { e.preventDefault(); f[f.length - 1].focus(); }
    else if (!e.shiftKey && document.activeElement === f[f.length - 1]) { e.preventDefault(); f[0].focus(); }
  });
  d.querySelector("input, textarea, select, button.btn")?.focus();
  return close;
}

function confirmAction(title, detail, run) {
  if (S.meta.readOnly) { toast(roMsg, true); return; }
  const typed = h("input", { type: "text", placeholder: S.sel.name, autocomplete: "off", "aria-label": "Type the object name to confirm" });
  const danger = isDanger();
  dialog(title, [h("p", { text: detail }), danger ? h("p", { class: "h-err", text: `${S.ctx} is production-like. Type the name to confirm:` }) : null, danger ? typed : null], (close) => [
    h("button", { class: "ghost", onclick: close }, "Cancel"),
    h("button", { class: "btn danger", onclick: async () => {
      if (danger && typed.value !== S.sel.name) { toast("The name does not match", true); return; }
      close();
      try { const r = await run(typed.value || S.sel.name); toast("✓ " + r.status); loadList(false); } catch (e) { toast(e.message, true); }
    } }, "Confirm")]);
}

const act = (body) => post("action", { ...targetBody(), ...body });

function actScale() {
  if (S.meta.readOnly) { toast(roMsg, true); return; }
  const n = h("input", { type: "number", min: "0", max: "1000", value: "1", "aria-label": "Replicas" });
  dialog(`Scale ${S.sel.name}`, [h("p", { text: "Replicas:" }), n], (close) => [h("button", { class: "ghost", onclick: close }, "Cancel"),
    h("button", { class: "btn", onclick: () => { close(); confirmAction(`Scale ${S.sel.name} to ${n.value}?`, `Changes the replica count on ${S.ctx}.`, (c) => act({ Action: "scale", Replicas: +n.value, Confirm: c })); } }, "Next")]);
}
const actRestart = () => confirmAction(`Restart ${S.sel.name}?`, `Rolls out new pods on ${S.ctx}, one batch at a time.`, (c) => act({ Action: "restart", Confirm: c }));
const actDelete = () => confirmAction(`Delete ${S.sel.name}?`, `Deletes the ${resInfo(S.sel.res).kind} on ${S.ctx}. This cannot be undone.`, (c) => act({ Action: "delete", Confirm: c }));

async function actForward() {
  if (S.meta.readOnly) { toast(roMsg, true); return; }
  try {
    const f = await post("forward", targetBody());
    toast("Forwarding " + f.desc);
    window.open(f.url, "_blank", "noopener");
    refreshForwards();
  } catch (e) { toast(e.message, true); }
}

async function refreshForwards() {
  const box = $("#forwards");
  try {
    const fs = await api("forwards");
    box.replaceChildren(...fs.map((f) => h("span", { class: "fwd" }, h("a", { href: f.url, target: "_blank", rel: "noopener noreferrer", text: "⇄ " + f.desc }),
      h("button", { class: "ghost tiny", "aria-label": "Stop " + f.desc, onclick: async () => { await post("forward/stop?id=" + f.id, {}).catch(() => {}); refreshForwards(); } }, "✕"))));
  } catch (_) { box.replaceChildren(); }
}

// ---------- palette & help ----------

function palette() {
  const items = [
    ...S.resources.map((r) => ({ label: r.kind, hint: r.group, terms: [r.name, r.short].join(" "), run: () => switchRes(r.name) })),
    ...S.meta.contexts.map((c) => ({ label: "cluster: " + c.name, hint: c.danger ? "production-like" : "", run: () => { S.ctx = c.name; loadContext(true); } })),
    ...Array.from($("#ns").options).map((o) => ({ label: "namespace: " + o.value, hint: "", run: () => setNS(o.value) })),
    { label: "Health check", hint: "detected issues", run: showHealth },
    { label: "What changed?", hint: "rollouts, config, scaling, Helm, GitOps in the last 2h", run: showChanges },
    { label: "SLOs", hint: "error budgets", run: showSLOs }, { label: "Snapshots", hint: "save and compare a namespace", run: showSnapshots },
    S.meta.ai ? { label: "Ask the AI", hint: "chat about this namespace", run: openChat } : null,
    { label: "Terminal UI", hint: "the full kubeweave in this tab", run: () => { location.href = $("#termLink").href; } },
    { label: "Toggle light / dark", hint: "", run: toggleTheme },
  ].filter(Boolean);
  const input = h("input", { type: "text", placeholder: "Type a resource, cluster or namespace…", autocomplete: "off", role: "combobox", "aria-expanded": "true", "aria-controls": "plist", "aria-label": "Go to" });
  const list = h("ul", { class: "plist", id: "plist", role: "listbox" });
  let cur = 0, shown = [];
  const render = () => {
    const words = input.value.toLowerCase().split(/\s+/).filter(Boolean);
    shown = items.filter((i) => words.every((w) => [i.label, i.hint, i.terms || ""].join(" ").toLowerCase().includes(w))).slice(0, 12);
    cur = Math.min(cur, Math.max(0, shown.length - 1));
    list.replaceChildren(...(shown.length ? shown.map((i, k) => h("li", { class: k === cur ? "cur" : "", role: "option", "aria-selected": String(k === cur), onclick: () => { close(); i.run(); } }, i.label, h("span", { class: "muted", text: i.hint })))
      : [h("li", { class: "muted", text: "No matches" })]));
  };
  input.addEventListener("input", () => { cur = 0; render(); });
  input.addEventListener("keydown", (e) => {
    if (e.key === "ArrowDown") { cur = Math.min(cur + 1, shown.length - 1); render(); e.preventDefault(); }
    if (e.key === "ArrowUp") { cur = Math.max(cur - 1, 0); render(); e.preventDefault(); }
    if (e.key === "Enter") { e.preventDefault(); e.stopPropagation(); if (shown[cur]) { close(); shown[cur].run(); } }
  });
  const close = dialog("Go to…", [input, list]);
  render();
}

function help() {
  const keys = [["j / k  ↑ / ↓", "move"], ["enter", "open details"], ["esc", "close"], ["/", "filter rows (server-side)"], ["1 – 9", "favourite resources"],
    ["space / x", "select row (bulk actions, compare two)"], ["Ctrl+K", "go to anything"], ["d y e t w l m a", "describe, YAML, edit, timeline, relations, logs, metrics, AI"],
    ["A", "chat with the AI"], ["H · C", "health check · what changed"], ["S (snapshots)", "save a snapshot"], ["r", "refresh"], ["n · c", "namespace · cluster"],
    ["S · R · F · Ctrl+D", "scale · restart · port-forward · delete (with --allow-changes)"], ["Ctrl+Enter", "dry-run in the editor"], ["?", "this help"]];
  dialog("Keyboard shortcuts", h("div", { class: "keys" }, ...keys.flatMap(([k, v]) => [h("kbd", { text: k }), h("span", { text: v })])),
    (close) => [h("button", { class: "btn", onclick: close }, "Close")]);
}

function toggleTheme() {
  const root = document.documentElement;
  const dark = root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
  root.dataset.theme = dark ? "light" : "dark";
  try { localStorage.setItem("kubeweave-theme", root.dataset.theme); } catch (_) {}
}

function refreshView() {
  ({ "@health": showHealth, "@changes": showChanges, "@slos": showSLOs, "@snapshots": showSnapshots }[S.res] || (() => loadList(true)))();
}

function setNS(ns) {
  S.ns = ns; $("#ns").value = ns; closePanel(); S.picked.clear();
  refreshView();
}

// ---------- wiring ----------

function wireChrome() {
  try { const t = localStorage.getItem("kubeweave-theme"); if (t) document.documentElement.dataset.theme = t; } catch (_) {}
  $("#ctx").onchange = (e) => { S.ctx = e.target.value; closePanel(); S.picked.clear(); loadContext(true); };
  $("#ns").onchange = (e) => setNS(e.target.value);
  let deb;
  $("#filter").addEventListener("input", (e) => { S.filter = e.target.value; S.cursor = 0; clearTimeout(deb); deb = setTimeout(() => loadList(false), 250); });
  $("#filter").addEventListener("keydown", (e) => { if (e.key === "Escape" || e.key === "Enter") { e.target.blur(); if (e.key === "Escape" && S.filter) { S.filter = ""; e.target.value = ""; loadList(false); } } });
  $("#refreshBtn").onclick = () => refreshView();
  $("#more").onclick = () => { S.limit += 500; loadList(false); };
  $("#paletteBtn").onclick = palette;
  $("#helpBtn").onclick = help;
  $("#chatBtn").onclick = openChat;
  $("#pclose").onclick = closePanel;
  document.addEventListener("keydown", onKey);
  setInterval(refreshForwards, 15000);
  window.addEventListener("hashchange", async () => { // a deep link pasted into an open tab
    const l = readHash();
    const ctxChanged = l.ctx && l.ctx !== S.ctx && S.meta.contexts.some((c) => c.name === l.ctx);
    if (ctxChanged) S.ctx = l.ctx;
    if (l.ns && l.ns !== S.ns) S.ns = l.ns;
    if (l.res && l.res !== S.res) { S.res = l.res; S.picked.clear(); S.filter = ""; $("#filter").value = ""; }
    closePanel();
    if (ctxChanged) await loadContext(false); else { $("#ns").value = S.ns; renderSide(); if (isSpecial()) refreshView(); else await loadList(true); }
    if (l.selName) openPanel({ ns: l.selNS, name: l.selName }, l.tab || "describe");
  });
}

function onKey(e) {
  if (e.defaultPrevented) return;
  const typing = /^(INPUT|SELECT|TEXTAREA)$/.test(document.activeElement?.tagName);
  const overlay = !$("#overlay").classList.contains("hidden");
  if (e.key === "Escape") {
    if (overlay) { $("#overlay").classList.add("hidden"); $("#overlay").replaceChildren(); lastFocus?.focus?.(); return; }
    if (S.sel && !typing) { closePanel(); return; }
  }
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") { e.preventDefault(); palette(); return; }
  if (typing || overlay || e.altKey || e.metaKey) return;
  if (e.ctrlKey && e.key.toLowerCase() === "d" && S.sel) { e.preventDefault(); actDelete(); return; }
  if (e.ctrlKey) return;
  const k = e.key;
  const move = (d) => { S.cursor = Math.max(0, Math.min(S.cursor + d, S.view.length - 1)); markCursor(); if (S.sel && current()) openPanel(current(), S.tab); e.preventDefault(); };
  if (k === "j" || k === "ArrowDown") move(1);
  else if (k === "k" || k === "ArrowUp") move(-1);
  else if (k === "Enter" && current()) openPanel(current(), S.tab);
  else if (k === "/") { e.preventDefault(); $("#filter").focus(); }
  else if (k === "?") help();
  else if (k === "r") refreshView();
  else if (k === "C") showChanges();
  else if (k === "S" && S.res === "@snapshots" && !S.sel) saveSnapshot();
  else if (k === "H") showHealth();
  else if (k === "A" && S.meta.ai) openChat();
  else if (k === "n") $("#ns").focus();
  else if (k === "c") $("#ctx").focus();
  else if ((k === " " || k === "x") && current() && !isSpecial()) { e.preventDefault(); const key = rowKey(current()); S.picked.has(key) ? S.picked.delete(key) : S.picked.add(key); renderTable(); }
  else if (/^[1-9]$/.test(k) && favorites()[+k - 1]) switchRes(favorites()[+k - 1]);
  else if (S.sel) {
    const tab = TABS.find((t) => t.key === k);
    if (tab && $("#tab-" + tab.id)) showTab(tab.id);
    else if (k === "S" && resInfo(S.sel.res).scale) actScale();
    else if (k === "R" && resInfo(S.sel.res).restart) actRestart();
    else if (k === "F") actForward();
  } else if (current()) {
    const tab = TABS.find((t) => t.key === k);
    if (tab) openPanel(current(), tab.id);
  }
}

boot();
