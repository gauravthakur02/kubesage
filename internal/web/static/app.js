// kubesage web UI. Plain JavaScript, no build step. Data comes from the kubesage JSON API (/api/*).
"use strict";

const TOKEN = window.KUBESAGE_TOKEN;
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

async function api(path, params, opts) {
  const q = new URLSearchParams(params || {});
  const res = await fetch(`/api/${path}?${q}`, { ...(opts || {}), headers: { "X-Kubesage-Token": TOKEN, "Content-Type": "application/json" } });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

function toast(msg, isErr) {
  const t = h("div", { class: "toast" + (isErr ? " err" : ""), text: msg });
  $("#toasts").append(t);
  setTimeout(() => t.remove(), isErr ? 8000 : 3500);
}

const S = {
  meta: null, ctx: "", ns: "all", resources: [], res: "pods",
  table: { columns: [], rows: [] }, filter: "", sort: { col: -1, dir: 1 }, view: [], cursor: 0,
  sel: null, tab: "describe", logSrc: null, timer: null, loading: false, collapsed: {},
};

const HEALTH_DOT = { ok: "●", warn: "▲", err: "✖", neutral: "○" };
const TABS = [
  { id: "describe", label: "Describe", key: "d" }, { id: "yaml", label: "YAML", key: "y" },
  { id: "timeline", label: "Timeline", key: "t", needs: (r) => r.graph }, { id: "graph", label: "Relations", key: "w", needs: (r) => r.graph },
  { id: "logs", label: "Logs", key: "l", needs: (r) => r.logs },
];

const resInfo = (name) => S.resources.find((r) => r.name === name) || { name, kind: name, namespaced: true };
const favorites = () => (S.meta.favorites && S.meta.favorites.length ? S.meta.favorites : ["pods", "deployments", "services", "events", "nodes"]);

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
    $("#title").textContent = "Cannot reach kubesage: " + e.message;
    return;
  }
  const link = readHash();
  S.ctx = link.ctx && S.meta.contexts.some((c) => c.name === link.ctx) ? link.ctx : S.meta.context;
  S.ns = link.ns || S.meta.namespace || "all";
  S.res = link.res || "pods";
  const mode = $("#mode");
  mode.textContent = S.meta.readOnly ? "READ-ONLY" : "CHANGES ALLOWED";
  mode.className = "badge " + (S.meta.readOnly ? "ro" : "rw");
  $("#termLink").href = "/term?t=" + encodeURIComponent(TOKEN);
  wireChrome();
  await loadContext(false);
  if (link.selName) openPanel({ ns: link.selNS, name: link.selName }, link.tab || "describe");
}

async function loadContext(resetNS) {
  const sel = $("#ctx");
  sel.replaceChildren(...S.meta.contexts.map((c) => h("option", { value: c.name, selected: c.name === S.ctx, text: (c.danger ? "⚠ " : "") + c.name })));
  const danger = S.meta.contexts.find((c) => c.name === S.ctx)?.danger;
  document.querySelector(".top").style.borderBottomColor = danger ? "var(--err)" : "";
  if (resetNS) S.ns = "all";
  const [res, ns] = await Promise.all([api("resources", { ctx: S.ctx }).catch(() => []), api("namespaces", { ctx: S.ctx }).catch((e) => ({ names: ["all"], note: e.message }))]);
  S.resources = res;
  const names = ns.names.includes(S.ns) ? ns.names : [...ns.names, S.ns];
  $("#ns").replaceChildren(...names.map((n) => h("option", { value: n, selected: n === S.ns, text: n })));
  if (ns.note && resetNS !== undefined) toast(ns.note, true);
  renderSide();
  await loadList(true);
}

// ---------- sidebar ----------

function renderSide() {
  const side = $("#side");
  const favs = favorites().map((n, i) => sideLink(resInfo(n), i < 9 ? String(i + 1) : ""));
  const groups = {};
  for (const r of S.resources) (groups[r.group || "Other APIs"] ||= []).push(r);
  const order = [...(S.meta.groups || []), ...Object.keys(groups).filter((g) => !(S.meta.groups || []).includes(g))];
  const parts = [h("a", { href: "#", class: S.res === "@health" ? "on health" : "health", onclick: (e) => { e.preventDefault(); showHealth(); } }, "⚕ Health check", h("kbd", { text: "H" })),
    h("h3", { text: "Favourites" }), ...favs];
  for (const g of order) {
    if (!groups[g]) continue;
    const closed = S.collapsed[g] ?? (g === "Custom resources" || g === "Other APIs");
    parts.push(h("h3", { onclick: () => { S.collapsed[g] = !closed; renderSide(); } }, g, h("span", { text: closed ? "+" : "−" })));
    if (!closed) parts.push(...groups[g].sort((a, b) => a.kind.localeCompare(b.kind)).map((r) => sideLink(r, "")));
  }
  side.replaceChildren(...parts);
}

function sideLink(r, key) {
  return h("a", { href: "#", class: r.name === S.res ? "on" : "", title: r.apiGroup ? r.name : r.kind,
    onclick: (e) => { e.preventDefault(); switchRes(r.name); } }, r.short && key ? r.short : r.kind, key ? h("kbd", { text: key }) : null);
}

async function switchRes(name) {
  if (S.res === name) return;
  S.res = name; S.filter = ""; $("#filter").value = ""; S.sort = { col: -1, dir: 1 }; S.cursor = 0;
  closePanel();
  renderSide();
  await loadList(true);
}

// ---------- table ----------

async function loadList(showLoading) {
  clearTimeout(S.timer);
  if (S.res === "@health") return;
  const r = resInfo(S.res);
  $("#title").textContent = `${r.kind}${r.kind.endsWith("s") ? "" : "s"} · ${r.namespaced ? S.ns : "cluster"}`;
  if (showLoading) { $("#note").textContent = "loading…"; }
  try {
    const t = await api("list", { ctx: S.ctx, ns: S.ns, res: S.res });
    S.table = t;
    $("#note").textContent = t.note || `${t.rows.length} items`;
    $("#updated").textContent = "updated " + new Date().toLocaleTimeString();
    renderTable();
  } catch (e) {
    S.table = { columns: [], rows: [] };
    renderTable(e.message);
  }
  writeHash();
  S.timer = setTimeout(() => { if (!document.hidden) loadList(false); else S.timer = setTimeout(() => loadList(false), 5000); }, 5000);
}

function computeView() {
  const f = S.filter.toLowerCase().trim();
  let rows = S.table.rows.filter((r) => !f || r.cells.join(" ").toLowerCase().includes(f) || (r.ns || "").toLowerCase().includes(f));
  const c = S.sort.col;
  if (c >= 0) {
    const num = (s) => { const m = /^(\d+(?:\.\d+)?)/.exec(s || ""); return m ? parseFloat(m[1]) : NaN; };
    rows = [...rows].sort((a, b) => {
      const x = a.cells[c] || "", y = b.cells[c] || "";
      const nx = num(x), ny = num(y);
      const d = !isNaN(nx) && !isNaN(ny) ? nx - ny : x.localeCompare(y, undefined, { numeric: true });
      return d * S.sort.dir;
    });
  }
  S.view = rows;
  S.cursor = Math.min(S.cursor, Math.max(0, rows.length - 1));
}

function renderTable(err) {
  computeView();
  const thead = $("#table thead"), tbody = $("#table tbody");
  thead.replaceChildren(h("tr", {}, h("th", { class: "dot" }), ...S.table.columns.map((c, i) => h("th", {
    class: S.sort.col === i ? "sorted" : "", title: "Sort",
    onclick: () => { S.sort = S.sort.col === i ? { col: i, dir: -S.sort.dir } : { col: i, dir: 1 }; renderTable(); },
  }, c, S.sort.col === i ? (S.sort.dir > 0 ? " ▲" : " ▼") : ""))));
  const sel = S.sel ? S.sel.ns + "/" + S.sel.name : null;
  tbody.replaceChildren(...S.view.map((r, i) => h("tr", {
    class: i === S.cursor ? "cur" : "", "data-i": i,
    onclick: () => { S.cursor = i; markCursor(); if (S.sel) openPanel(r, S.tab); },
    ondblclick: () => openPanel(r, S.tab),
  }, h("td", { class: "dot h-" + r.health, text: HEALTH_DOT[r.health] || "" }), ...r.cells.map((c) => h("td", { text: c, title: c.length > 40 ? c : null })))));
  const empty = $("#empty");
  empty.classList.toggle("hidden", S.view.length > 0);
  empty.textContent = err ? "✗ " + err : S.table.rows.length ? "No rows match the filter." : "Nothing here.";
  if (sel) {
    const i = S.view.findIndex((r) => r.ns + "/" + r.name === sel);
    if (i >= 0) { S.cursor = i; markCursor(); }
  }
}

function markCursor() {
  document.querySelectorAll("#table tbody tr").forEach((tr, i) => tr.classList.toggle("cur", i === S.cursor));
  const cur = document.querySelector("#table tbody tr.cur");
  if (cur) cur.scrollIntoView({ block: "nearest" });
}

const current = () => S.view[S.cursor];

// ---------- health ----------

async function showHealth() {
  clearTimeout(S.timer);
  S.res = "@health"; closePanel(); renderSide();
  $("#title").textContent = "Health check · " + S.ns;
  $("#note").textContent = "scanning…";
  try {
    const is = await api("issues", { ctx: S.ctx, ns: S.ns });
    S.table = { columns: ["SEVERITY", "KIND", "NAMESPACE", "NAME", "PROBLEM", "WHAT TO DO"],
      rows: is.map((i) => ({ ns: i.NS, name: i.Name, kind: i.Kind, health: i.Severity, cells: [i.Severity === "err" ? "error" : "warning", i.Kind, i.NS, i.Name, i.Reason + ": " + i.Message, i.Hint] })) };
    $("#note").textContent = is.length ? `${is.length} issues` : "✓ no issues found";
  } catch (e) { S.table = { columns: [], rows: [] }; renderTable(e.message); return; }
  renderTable();
}

// ---------- detail panel ----------

function openPanel(row, tab) {
  const resName = S.res === "@health" ? row.kind : S.res;
  const r = resInfo(resName);
  S.sel = { ns: row.ns, name: row.name, res: r.name };
  const tabs = TABS.filter((t) => !t.needs || t.needs(r));
  if (!tabs.some((t) => t.id === tab)) tab = "describe";
  $("#panel").classList.remove("hidden");
  $("#pkind").textContent = r.kind + (row.ns ? " · " + row.ns : "");
  $("#pname").textContent = row.name;
  $("#ptabs").replaceChildren(...tabs.map((t) => h("button", { class: t.id === tab ? "on" : "", role: "tab", onclick: () => showTab(t.id) }, t.label, h("kbd", { text: t.key }))));
  const ro = S.meta.readOnly, why = ro ? "Read-only: start kubesage serve with --allow-changes" : null;
  $("#pactions").replaceChildren(...[
    r.scale ? h("button", { class: "ghost", disabled: ro, title: why || "Scale (S)", onclick: () => actScale() }, "Scale") : null,
    r.restart ? h("button", { class: "ghost", disabled: ro, title: why || "Rollout restart (R)", onclick: () => actRestart() }, "Restart") : null,
    h("button", { class: "ghost", disabled: ro, title: why || "Delete (Ctrl+D)", onclick: () => actDelete() }, "Delete"),
    h("button", { class: "ghost", title: "Copy a link to this view", onclick: () => { navigator.clipboard?.writeText(location.href.replace(/\?t=[^#]*/, "")); toast("Link copied (open it from a kubesage serve page; the token is not included)"); } }, "Link")].filter(Boolean));
  showTab(tab);
}

function closePanel() {
  stopLogs();
  S.sel = null;
  $("#panel").classList.add("hidden");
  writeHash();
}

const target = () => ({ ctx: S.ctx, ns: S.sel.ns || "", res: S.sel.res, name: S.sel.name });

async function showTab(tab) {
  stopLogs();
  S.tab = tab;
  document.querySelectorAll("#ptabs button").forEach((b) => b.classList.toggle("on", b.textContent.startsWith(TABS.find((t) => t.id === tab).label)));
  writeHash();
  const body = $("#pbody");
  body.replaceChildren(h("div", { class: "muted", text: "loading…" }));
  const sel = S.sel;
  try {
    if (tab === "describe" || tab === "yaml") {
      const out = await api(tab, target());
      if (S.sel !== sel || S.tab !== tab) return;
      body.replaceChildren(h("div", { class: "toolbar" }, h("button", { class: "ghost", onclick: () => { navigator.clipboard?.writeText(out.text); toast("Copied"); } }, "Copy"),
        tab === "yaml" ? h("span", { class: "muted", text: "Secret values are masked." }) : null), codeBlock(out.text, tab === "yaml"));
    } else if (tab === "timeline") {
      const tl = await api("timeline", target());
      if (S.sel !== sel || S.tab !== tab) return;
      body.replaceChildren(tl.length ? h("ul", { class: "tl" }, ...tl.map((e) => h("li", {},
        h("span", { class: "muted", text: new Date(e.Time).toLocaleString() }), h("span", { class: "src h-" + e.Health, text: e.Source }),
        h("span", {}, h("b", { text: e.Object + " " }), e.Text, e.Count > 1 ? h("span", { class: "muted", text: ` ×${e.Count}` }) : null)))) : h("div", { class: "muted", text: "Nothing in the timeline." }));
    } else if (tab === "graph") {
      const g = await api("graph", target());
      if (S.sel !== sel || S.tab !== tab) return;
      const parts = [];
      if (g.front && g.front.children && g.front.children.length) parts.push(h("div", { class: "muted", text: "Reached through" }), h("ul", { class: "tree" }, ...g.front.children.map(treeNode)));
      parts.push(h("div", { class: "muted", text: "Made of" }), h("ul", { class: "tree" }, treeNode(g.root)));
      body.replaceChildren(...parts);
    } else if (tab === "logs") {
      await showLogs(body);
    }
  } catch (e) {
    body.replaceChildren(h("div", { class: "h-err", text: "✗ " + e.message }));
  }
}

const KIND_RES = { Pod: "pods", Deployment: "deployments", StatefulSet: "statefulsets", DaemonSet: "daemonsets", ReplicaSet: "replicasets",
  Service: "services", Ingress: "ingresses", ConfigMap: "configmaps", Secret: "secrets", PersistentVolumeClaim: "persistentvolumeclaims", HPA: "horizontalpodautoscalers" };

function treeNode(n) {
  if (!n) return null;
  const res = KIND_RES[n.kind];
  return h("li", {}, h("span", { class: "node", title: res ? "Open" : "", onclick: res ? async () => {
    if (S.res !== res) { S.res = res; renderSide(); await loadList(true); }
    openPanel({ ns: n.ns, name: n.name.split(" ")[0] }, "describe");
  } : null }, h("span", { class: "h-" + n.health, text: (HEALTH_DOT[n.health] || "") + " " }), h("span", { class: "kind", text: n.kind }), n.name,
    n.status ? h("span", { class: "muted", text: "  " + n.status }) : null, n.note ? h("span", { class: "h-warn", text: "  " + n.note }) : null),
    n.children && n.children.length ? h("ul", {}, ...n.children.map(treeNode)) : null);
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

// ---------- logs ----------

async function showLogs(body) {
  const sel = S.sel;
  const pods = await api("pods", target());
  if (S.sel !== sel) return;
  if (!pods.length) { body.replaceChildren(h("div", { class: "muted", text: "No pods." })); return; }
  const podSel = h("select", {}, ...pods.map((p) => h("option", { value: p.name, text: p.name })));
  const ctrSel = h("select", {});
  const fillCtr = () => { const p = pods.find((x) => x.name === podSel.value); ctrSel.replaceChildren(...p.containers.map((c) => h("option", { value: c, text: c }))); };
  fillCtr();
  const search = h("input", { type: "search", placeholder: "highlight…" });
  const follow = h("input", { type: "checkbox", checked: true });
  const prev = h("input", { type: "checkbox" });
  const out = h("div", { class: "logs" });
  const status = h("span", { class: "muted" });
  const start = () => {
    stopLogs();
    out.replaceChildren();
    const q = new URLSearchParams({ ctx: S.ctx, ns: sel.ns, pod: podSel.value, container: ctrSel.value, tail: "500", previous: prev.checked ? "1" : "0", t: TOKEN });
    const es = new EventSource("/api/logs?" + q);
    S.logSrc = es;
    status.textContent = "● streaming";
    let n = 0;
    es.onmessage = (e) => {
      const line = JSON.parse(e.data);
      const cls = /\b(error|fatal|panic|exception)\b/i.test(line) ? "err" : /\bwarn(ing)?\b/i.test(line) ? "warn" : "";
      const div = h("div", { class: cls });
      const term = search.value.trim();
      if (term) {
        const i = line.toLowerCase().indexOf(term.toLowerCase());
        if (i >= 0) div.append(line.slice(0, i), h("mark", { text: line.slice(i, i + term.length) }), line.slice(i + term.length));
        else div.textContent = line;
      } else div.textContent = line;
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
  body.replaceChildren(h("div", { class: "toolbar" }, podSel, ctrSel, search, h("label", {}, follow, " follow"), h("label", {}, prev, " previous"),
    h("button", { class: "ghost", onclick: () => { navigator.clipboard?.writeText(out.innerText); toast("Copied"); } }, "Copy"), status), out);
  start();
}

function stopLogs() {
  if (S.logSrc) { S.logSrc.close(); S.logSrc = null; }
}

// ---------- actions ----------

function dialog(title, content, buttons) {
  const ov = $("#overlay");
  const close = () => { ov.classList.add("hidden"); ov.replaceChildren(); };
  const d = h("div", { class: "dialog", role: "dialog" }, h("h3", { text: title }), h("div", { class: "dbody" }, content),
    buttons ? h("div", { class: "dfoot" }, ...buttons(close)) : null);
  ov.classList.remove("hidden");
  ov.replaceChildren(d);
  ov.onclick = (e) => { if (e.target === ov) close(); };
  const first = d.querySelector("input, select, button.btn");
  if (first) first.focus(); // at once, so keys typed right after the shortcut land in the dialog
  return close;
}

const isDanger = () => S.meta.contexts.find((c) => c.name === S.ctx)?.danger;

function confirmAction(title, detail, run) {
  if (S.meta.readOnly) { toast("Read-only: start kubesage serve with --allow-changes", true); return; }
  const typed = h("input", { type: "text", placeholder: S.sel.name, autocomplete: "off" });
  const danger = isDanger();
  dialog(title, [h("p", { text: detail }), danger ? h("p", { class: "h-err", text: `${S.ctx} is production-like. Type the name to confirm:` }) : null, danger ? typed : null], (close) => [
    h("button", { class: "ghost", onclick: close }, "Cancel"),
    h("button", { class: "btn danger", onclick: async () => {
      if (danger && typed.value !== S.sel.name) { toast("The name does not match", true); return; }
      close();
      try { const r = await run(typed.value); toast("✓ " + r.status); loadList(false); } catch (e) { toast(e.message, true); }
    } }, "Confirm")]);
}

const post = (body) => api("action", {}, { method: "POST", body: JSON.stringify({ ...body, Ctx: S.ctx, NS: S.sel.ns, Res: S.sel.res, Name: S.sel.name }) });

function actScale() {
  if (S.meta.readOnly) { toast("Read-only: start kubesage serve with --allow-changes", true); return; }
  const n = h("input", { type: "number", min: "0", max: "1000", value: "1" });
  dialog(`Scale ${S.sel.name}`, [h("p", { text: "Replicas:" }), n], (close) => [h("button", { class: "ghost", onclick: close }, "Cancel"),
    h("button", { class: "btn", onclick: () => { close(); confirmAction(`Scale ${S.sel.name} to ${n.value}?`, `Changes the replica count on ${S.ctx}.`, (c) => post({ Action: "scale", Replicas: +n.value, Confirm: c })); } }, "Next")]);
}
const actRestart = () => confirmAction(`Restart ${S.sel.name}?`, `Rolls out new pods on ${S.ctx}, one batch at a time.`, (c) => post({ Action: "restart", Confirm: c }));
const actDelete = () => confirmAction(`Delete ${S.sel.name}?`, `Deletes the ${resInfo(S.sel.res).kind} on ${S.ctx}. This cannot be undone.`, (c) => post({ Action: "delete", Confirm: c }));

// ---------- palette & help ----------

function palette() {
  const items = [
    ...S.resources.map((r) => ({ label: r.kind, hint: r.group, terms: [r.name, r.short].join(" "), run: () => switchRes(r.name) })),
    ...S.meta.contexts.map((c) => ({ label: "cluster: " + c.name, hint: c.danger ? "production-like" : "", run: () => { S.ctx = c.name; loadContext(true); } })),
    ...Array.from($("#ns").options).map((o) => ({ label: "namespace: " + o.value, hint: "", run: () => setNS(o.value) })),
    { label: "Health check", hint: "detected issues", run: showHealth },
    { label: "Terminal UI", hint: "the full kubesage in this tab", run: () => { location.href = $("#termLink").href; } },
    { label: "Toggle light / dark", hint: "", run: toggleTheme },
  ];
  const input = h("input", { type: "text", placeholder: "Type a resource, cluster or namespace…", autocomplete: "off" });
  const list = h("ul", { class: "plist" });
  let cur = 0, shown = [];
  const render = () => {
    const words = input.value.toLowerCase().split(/\s+/).filter(Boolean);
    shown = items.filter((i) => words.every((w) => [i.label, i.hint, i.terms || ""].join(" ").toLowerCase().includes(w))).slice(0, 12);
    cur = Math.min(cur, Math.max(0, shown.length - 1));
    list.replaceChildren(...(shown.length ? shown.map((i, k) => h("li", { class: k === cur ? "cur" : "", onclick: () => { close(); i.run(); } }, i.label, h("span", { class: "muted", text: i.hint })))
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
  const keys = [["j / k  ↑ / ↓", "move"], ["enter", "open details"], ["esc", "close"], ["/", "filter rows"], ["1 – 9", "favourite resources"],
    ["Ctrl+K", "go to anything"], ["d y t w l", "describe, YAML, timeline, relations, logs"], ["H", "health check"], ["r", "refresh"],
    ["n · c", "namespace · cluster"], ["S · R · Ctrl+D", "scale · restart · delete (with --allow-changes)"], ["?", "this help"]];
  dialog("Keyboard shortcuts", h("div", { class: "keys" }, ...keys.flatMap(([k, v]) => [h("kbd", { text: k }), h("span", { text: v })])),
    (close) => [h("button", { class: "btn", onclick: close }, "Close")]);
}

function toggleTheme() {
  const root = document.documentElement;
  const dark = root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
  root.dataset.theme = dark ? "light" : "dark";
  try { localStorage.setItem("kubesage-theme", root.dataset.theme); } catch (_) {}
}

function setNS(ns) {
  S.ns = ns; $("#ns").value = ns; closePanel();
  if (S.res === "@health") showHealth(); else loadList(true);
}

// ---------- wiring ----------

function wireChrome() {
  try { const t = localStorage.getItem("kubesage-theme"); if (t) document.documentElement.dataset.theme = t; } catch (_) {}
  $("#ctx").onchange = (e) => { S.ctx = e.target.value; closePanel(); loadContext(true); };
  $("#ns").onchange = (e) => setNS(e.target.value);
  $("#filter").addEventListener("input", (e) => { S.filter = e.target.value; S.cursor = 0; renderTable(); });
  $("#filter").addEventListener("keydown", (e) => { if (e.key === "Escape" || e.key === "Enter") { e.target.blur(); if (e.key === "Escape") { S.filter = ""; e.target.value = ""; renderTable(); } } });
  $("#refreshBtn").onclick = () => (S.res === "@health" ? showHealth() : loadList(true));
  $("#paletteBtn").onclick = palette;
  $("#helpBtn").onclick = help;
  $("#pclose").onclick = closePanel;
  document.addEventListener("keydown", onKey);
}

function onKey(e) {
  if (e.defaultPrevented) return;
  const typing = /^(INPUT|SELECT|TEXTAREA)$/.test(document.activeElement?.tagName);
  const overlay = !$("#overlay").classList.contains("hidden");
  if (e.key === "Escape") {
    if (overlay) { $("#overlay").classList.add("hidden"); $("#overlay").replaceChildren(); return; }
    if (S.sel) { closePanel(); return; }
  }
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") { e.preventDefault(); palette(); return; }
  if (typing || overlay || e.altKey || (e.metaKey && e.key !== "Enter")) return;
  if (e.ctrlKey && e.key.toLowerCase() === "d" && S.sel) { e.preventDefault(); actDelete(); return; }
  if (e.ctrlKey) return;
  const k = e.key;
  if (k === "j" || k === "ArrowDown") { S.cursor = Math.min(S.cursor + 1, S.view.length - 1); markCursor(); if (S.sel && current()) openPanel(current(), S.tab); e.preventDefault(); }
  else if (k === "k" || k === "ArrowUp") { S.cursor = Math.max(S.cursor - 1, 0); markCursor(); if (S.sel && current()) openPanel(current(), S.tab); e.preventDefault(); }
  else if (k === "Enter" && current()) openPanel(current(), S.tab);
  else if (k === "/") { e.preventDefault(); $("#filter").focus(); }
  else if (k === "?") help();
  else if (k === "r") (S.res === "@health" ? showHealth() : loadList(true));
  else if (k === "H") showHealth();
  else if (k === "n") $("#ns").focus();
  else if (k === "c") $("#ctx").focus();
  else if (/^[1-9]$/.test(k) && favorites()[+k - 1]) switchRes(favorites()[+k - 1]);
  else if (S.sel) {
    const tab = TABS.find((t) => t.key === k);
    if (tab && document.querySelector("#ptabs").textContent.includes(tab.label)) showTab(tab.id);
    else if (k === "S" && resInfo(S.sel.res).scale) actScale();
    else if (k === "R" && resInfo(S.sel.res).restart) actRestart();
  } else if (current()) {
    const tab = TABS.find((t) => t.key === k);
    if (tab) openPanel(current(), tab.id);
  }
}

boot();
