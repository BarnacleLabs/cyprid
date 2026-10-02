// cyprid web UI. Plain DOM, no framework. Every piece of server or
// model text goes in through textContent, never innerHTML.

// ---------- helpers ----------

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "style" && typeof v === "object") Object.assign(el.style, v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}
const $ = (sel) => document.querySelector(sel);

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  let data = null;
  try { data = await resp.json(); } catch { /* empty body */ }
  if (resp.status === 401 && path.startsWith("/admin")) $("#signin").hidden = false; // not signed in
  if (!resp.ok) {
    const err = new Error((data && data.detail) || `${resp.status} ${resp.statusText}`);
    err.status = resp.status;
    err.data = data;
    throw err;
  }
  return data;
}

let toastTimer;
function toast(msg, bad = false) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "toast" + (bad ? " bad" : "");
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), bad ? 7000 : 3000);
}

const pct = (v) => (v == null ? "–" : (v * 100).toFixed(1) + "%");
const gb = (v) => (v ? `~${v.toFixed(0)} GB` : "–");
function ago(iso) {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (s < 60) return `${Math.round(s)}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  return `${Math.round(s / 3600)}h ago`;
}

// ---------- show in Finder ----------

async function showInFinder(what, name = "") {
  try { await api("POST", "/admin/reveal", { what, name }); }
  catch (e) { toast(e.message, true); }
}
function finderButton(what, name, label = "Show in Finder") {
  return h("button", { class: "btn small", onclick: (ev) => { ev.stopPropagation(); showInFinder(what, name); } }, label);
}

// ---------- tabs ----------

let currentTab = "models";
function showTab(name) {
  currentTab = name;
  for (const b of document.querySelectorAll(".tabs button")) b.setAttribute("aria-selected", b.dataset.tab === name);
  for (const p of document.querySelectorAll(".tab-panel")) p.hidden = p.id !== "tab-" + name;
  if (name === "playground") renderPlayModels();
  if (name === "recipes") loadRecipes();
  if (name === "bench") loadBench();
  history.replaceState(null, "", "#" + name);
}
for (const b of document.querySelectorAll(".tabs button")) b.addEventListener("click", () => showTab(b.dataset.tab));

// ---------- status + models ----------

let models = [];
let status = {};

async function refresh() {
  try {
    const [m, s] = await Promise.all([api("GET", "/v1/models"), api("GET", "/admin/status")]);
    models = m.models || [];
    status = s;
    $("#banner").hidden = !s.recipe_error;
    $("#banner").textContent = s.recipe_error ? `A recipe file has an error, so the previous recipes are still in use: ${s.recipe_error}` : "";
    if (!s.uv) {
      $("#banner").hidden = false;
      $("#banner").textContent = "uv was not found on the server's PATH, so models can't be installed. Install it with: brew install uv";
    }
    $("#addr").textContent = s.addr || location.host;
    if (s.version) {
      const v = s.version;
      $("#version").textContent = v.version === "dev" && v.commit ? `dev ${v.commit.slice(0, 7)}` : v.version;
      $("#version").title = `cyprid ${v.version}${v.commit ? `, commit ${v.commit.slice(0, 12)}` : ""}${v.modified ? ", with uncommitted changes" : ""}; ${v.go}`;
    }
    const used = s.used_mem_gb || 0, max = s.max_mem_gb || 0;
    $("#mem-text").textContent = max ? `${used.toFixed(0)} / ${max.toFixed(0)} GB` : `${used.toFixed(0)} GB`;
    $("#mem-fill").style.width = max ? Math.min(100, (used / max) * 100) + "%" : "0";
    const home = (s.home || "~/.cyprid").replace(/^\/Users\/[^/]+/, "~");
    $("#recipes-dir").textContent = home + "/recipes";
    $("#data-dir").textContent = home;
    renderNetPill();
    renderModels();
    if (drawerModel) refreshDrawer();
  } catch (e) {
    $("#banner").hidden = false;
    $("#banner").textContent = "Can't reach the cyprid server: " + e.message;
  }
}

function stateBadge(m) {
  if (m.install_job && m.install_job.state === "running") {
    const running = { install: "installing", uninstall: "uninstalling", "delete-weights": "deleting weights" }[m.install_job.kind] || m.install_job.kind;
    return h("span", { class: "badge accent pulse" }, h("span", { class: "dot" }), running);
  }
  const map = {
    ready: ["ok", "ready"], starting: ["warn pulse", "loading"], stopping: ["warn pulse", "stopping"],
    failed: ["bad", "failed"], stopped: ["", "stopped"], remote: ["accent", "remote"],
  };
  const [cls, label] = map[m.state] || ["", m.state];
  return h("span", { class: "badge " + cls }, h("span", { class: "dot" }), label);
}

function installBadge(m) {
  const map = {
    installed: ["ok", "installed"], "not installed": ["", "not installed"],
    outdated: ["warn", "outdated"], remote: ["accent", "hosted"],
  };
  const [cls, label] = map[m.installed] || ["", m.installed];
  return h("span", { class: "badge " + cls }, label);
}

function caps(m) {
  return h("div", { class: "caps" }, ["noul", "choice", "score"].map((t) => {
    const c = (m.capabilities || {})[t] || "native";
    const title = c === "native" ? `${t}: native` : `${t}: ${c}`;
    return h("span", { class: "cap " + c, title }, t);
  }));
}

const busy = new Set(); // model names with an action in flight

const gbytes = (n) => (n >= 1e9 ? `${(n / 1e9).toFixed(1)} GB` : n >= 1e6 ? `${Math.round(n / 1e6)} MB` : `${Math.round(n / 1e3)} KB`);

// removeDialog asks how to uninstall (or, for a model no longer installed, whether to delete its cached weights).
// It resolves to the action path to POST, or null if cancelled.
async function removeDialog(m, weightsOnly) {
  let plan;
  try { plan = await api("GET", `/admin/models/${encodeURIComponent(m.name)}/weights`); } catch (e) { toast(e.message, true); return null; }
  const repos = plan.repos || [];
  $("#rm-title").textContent = weightsOnly ? `Delete ${m.name}'s weights` : `Uninstall ${m.name}`;
  $("#rm-intro").textContent = weightsOnly
    ? "The model isn't installed; its weights are still in the Hugging Face cache."
    : "Stops the model and removes its Python environment (kept if another model of the same recipe still uses it).";
  const box = $("#rm-weights");
  box.checked = weightsOnly;
  $("#rm-weights-row").hidden = weightsOnly || plan.freeable_bytes === 0;
  $("#rm-weights-label").textContent = `Also delete downloaded weights (${gbytes(plan.freeable_bytes)})`;
  $("#rm-repos").replaceChildren(...repos.map((u) => h("li", {},
    h("span", {}, u.repo),
    h("span", { class: "kept" }, !u.present ? "not in cache" : u.shared_with ? `kept: used by ${u.shared_with.join(", ")}` : gbytes(u.bytes)))));
  const label = () => { $("#rm-confirm").textContent = weightsOnly ? `Delete ${gbytes(plan.freeable_bytes)}` : box.checked ? `Uninstall and free ${gbytes(plan.freeable_bytes)}` : "Uninstall"; $("#rm-repos").hidden = !box.checked; };
  box.onchange = label;
  label();
  const dlg = $("#rm-dialog");
  return new Promise((resolve) => {
    const done = (v) => { dlg.close(); resolve(v); };
    $("#rm-confirm").onclick = () => done(weightsOnly ? "delete-weights" : box.checked ? "uninstall?delete_weights=1" : "uninstall");
    $("#rm-cancel").onclick = () => done(null);
    $("#rm-close").onclick = () => done(null);
    dlg.onclose = () => resolve(null);
    dlg.showModal();
  });
}

async function act(m, action) {
  if (action === "uninstall" || action === "delete-weights") {
    action = await removeDialog(m, action === "delete-weights");
    if (!action) return;
  }
  busy.add(m.name);
  renderModels();
  try {
    await api("POST", `/admin/models/${encodeURIComponent(m.name)}/${action}`);
    const kind = action.split("?")[0];
    if (kind !== "start" && kind !== "stop") openDrawer(m.name, "job");
    toast({ start: `Starting ${m.name}`, stop: `Stopped ${m.name}`, install: `Installing ${m.name}`, uninstall: `Uninstalling ${m.name}`, "delete-weights": `Deleting ${m.name}'s weights` }[kind]);
  } catch (e) {
    toast(e.message, true);
  } finally {
    busy.delete(m.name);
    refresh();
  }
}

function actions(m) {
  const out = [];
  const b = (label, action, cls = "") =>
    h("button", { class: "btn small " + cls, disabled: busy.has(m.name), onclick: (ev) => { ev.stopPropagation(); act(m, action); } }, label);
  const jobRunning = m.install_job && m.install_job.state === "running";
  if (m.kind === "remote") {
    out.push(h("button", { class: "btn small", onclick: (ev) => { ev.stopPropagation(); tryInPlayground(m.name); } }, "Try"));
    return h("div", { class: "actions" }, out);
  }
  if (jobRunning) {
    out.push(h("button", { class: "btn small", onclick: (ev) => { ev.stopPropagation(); openDrawer(m.name, "job"); } }, "View output"));
  } else if (m.installed === "not installed") {
    out.push(b("Install", "install", tooBig(m) ? "" : "primary"));
    if (m.cached_weights_bytes > 0) out.push(b(`Delete weights (${gbytes(m.cached_weights_bytes)})`, "delete-weights", "danger"));
  } else {
    if (m.installed === "outdated") out.push(b("Reinstall", "install", "primary"));
    if (m.installed === "installed") {
      if (m.state === "ready" || m.state === "starting") out.push(b("Stop", "stop"));
      else if (m.state !== "stopping") out.push(b("Start", "start"));
      if (m.state === "ready") out.push(h("button", { class: "btn small", onclick: (ev) => { ev.stopPropagation(); tryInPlayground(m.name); } }, "Try"));
    }
    out.push(b("Uninstall", "uninstall", "danger"));
  }
  out.push(h("button", { class: "btn small", onclick: (ev) => { ev.stopPropagation(); openDrawer(m.name, jobRunning ? "job" : "logs"); } }, "Logs"));
  return h("div", { class: "actions" }, out);
}

const tooBig = (m) => status.max_mem_gb > 0 && m.mem_gb > status.max_mem_gb;

function stateDetail(m) {
  if (tooBig(m)) return h("div", { class: "state-detail bad", title: "Raise the budget with: cyprid serve --max-mem-gb N" }, `needs ~${m.mem_gb} GB; budget is ${status.max_mem_gb.toFixed(0)} GB`);
  if (m.install_job && m.install_job.state === "failed") return h("div", { class: "state-detail bad", title: m.install_job.error }, `${m.install_job.kind} failed: ${m.install_job.error}`);
  if (m.state === "failed" && m.error) return h("div", { class: "state-detail bad", title: m.error }, m.error);
  if (m.state === "ready") {
    const bits = [`port ${m.port}`];
    if (m.inflight) bits.push(`${m.inflight} in flight`);
    if (m.last_used) bits.push(`used ${ago(m.last_used)}`);
    return h("div", { class: "state-detail" }, bits.join(" · "));
  }
  if (m.state === "starting" && m.since) return h("div", { class: "state-detail" }, `started ${ago(m.since)}`);
  return null;
}

function contextCell(m) {
  const c = m.context;
  if (!c || !c.tokens) return "–";
  const n = c.tokens;
  const label = n >= 1024 && n % 1024 === 0 ? `${n / 1024}K` : n.toLocaleString();
  const short = n < 2048;
  return h("span", {
    class: "context" + (short ? " short" : ""),
    title: `${n.toLocaleString()} tokens${c.note ? ": " + c.note : ""}`,
  }, label);
}

function sourceLinks(m) {
  const links = (m.links || []).filter((l) => /^https?:\/\//.test(l.url)); // the server only allows http(s); check anyway
  if (!links.length && !m.license && !m.base) return null;
  const items = links.map((l) => h("a", {
    href: l.url, target: "_blank", rel: "noopener noreferrer", title: l.url,
    onclick: (ev) => ev.stopPropagation(), // don't also open the log drawer
  }, l.label));
  if (m.license) items.push(h("span", { class: "license", title: "Licence" }, m.license));
  if (m.base) items.push(h("span", { class: "license", title: "Foundation model and its maker" }, "built on " + m.base));
  return h("div", { class: "model-links" }, items.flatMap((el, i) => (i ? [h("span", { class: "sep" }, "·"), el] : [el])));
}

function calibrationLine(m) {
  const t = m.calibration_temperature;
  return h("div", { class: "model-cal" },
    t ? h("span", { class: "badge accent", title: "cyprid rescales this model's confidence" }, `calibrated · T=${t}`) : h("span", { class: "muted" }, "calibration off"),
    h("button", { class: "linkish", onclick: (ev) => { ev.stopPropagation(); openCalibration(m); } }, t ? "change" : "set…"));
}

async function openCalibration(m) {
  let info;
  try { info = await api("GET", `/admin/models/${encodeURIComponent(m.name)}/calibration`); } catch (e) { toast(e.message, true); return; }
  $("#cal-title").textContent = `Calibrate ${m.name}`;
  $("#cal-current").textContent = info.temperature ? `Now: on, temperature ${info.temperature} (${info.source}).` : `Now: ${info.source}.`;
  $("#cal-temp").value = info.temperature || "";
  $("#cal-error").hidden = true;
  $("#cal-reset").hidden = !(info.recipe_temperature > 0) || info.source === "recipe";
  const sugg = info.suggestions || [];
  $("#cal-suggestions").replaceChildren(...(sugg.length ? sugg.map((g) => h("li", {},
    h("span", {}, `${g.suite || g.run}: T=${g.temperature} `, h("span", { class: "kept" },
      `(n=${g.n}; ECE ${g.ece_before.toFixed(3)} → ${g.ece_after.toFixed(3)}; confident errors ${(g.confident_errors_before * 100).toFixed(1)}% → ${(g.confident_errors_after * 100).toFixed(1)}%)`)),
    h("button", { class: "btn small", onclick: () => { $("#cal-temp").value = g.temperature; } }, "Use"))) :
    [h("li", {}, h("span", { class: "kept" }, `No benchmark run of ${m.name} with at least 100 answered items yet. Run: cyprid bench --models ${m.name}`))]));
  const dlg = $("#cal-dialog");
  const put = async (temperature) => {
    try {
      const r = await api("PUT", `/admin/models/${encodeURIComponent(m.name)}/calibration`, { temperature });
      dlg.close();
      toast(r.temperature ? `${m.name}: calibrated, T=${r.temperature}` : `${m.name}: calibration ${r.source}`);
      refresh();
    } catch (e) { $("#cal-error").textContent = e.message; $("#cal-error").hidden = false; }
  };
  $("#cal-save").onclick = () => {
    const v = parseFloat($("#cal-temp").value);
    if (!(v > 0 && v <= 20)) { $("#cal-error").textContent = "Enter a temperature between 0 and 20 (for example 2.2)."; $("#cal-error").hidden = false; return; }
    put(v);
  };
  $("#cal-off").onclick = () => put(0);
  $("#cal-reset").onclick = () => put(null);
  $("#cal-close").onclick = () => dlg.close();
  dlg.showModal();
}

function versionLine(m) {
  const v = m.version;
  const bits = [], detail = [];
  const day = (iso) => new Date(iso).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
  for (const p of (v && v.packages) || []) {
    bits.push(`${p.name} ${p.version}`);
    detail.push(`${p.name} ${p.version}` + (p.commit ? ` (git ${p.git} @ ${p.commit})` : ""));
  }
  for (const w of (v && v.weights) || []) {
    if (!w.revision) { detail.push(`${w.repo}: ${w.error || "unknown revision"}`); continue; }
    const when = w.date && !w.date.startsWith("0001") ? ` (${day(w.date)})` : "";
    bits.push(`weights ${w.revision.slice(0, 7)}${when}`);
    detail.push(`${w.repo} revision ${w.revision}${when}${w.pinned ? "" : ", latest at install time (not pinned)"}`);
  }
  if (m.reported_model) {
    bits.push(`reports ${m.reported_model.name}`);
    detail.push(`The backend reported itself as ${m.reported_model.name} in its last response`);
  }
  if (!bits.length) return null;
  if (v && v.recorded_at) detail.push(`Recorded ${new Date(v.recorded_at).toLocaleString()}${v.backfilled ? " (from disk, after install)" : ""}`);
  return h("div", { class: "model-version", title: detail.join("\n") }, bits.join(" · "));
}

function renderModels() {
  const body = $("#models-body");
  body.replaceChildren(...models.map((m) =>
    h("tr", { class: drawerModel === m.name ? "selected" : "", onclick: () => openDrawer(m.name, "logs") },
      h("td", {},
        h("div", { class: "model-name" }, m.name, " ", (m.aliases || []).length ? h("span", { class: "alias" }, "(" + m.aliases.join(", ") + ")") : null),
        h("div", { class: "model-meta" }, m.description || `recipe ${m.recipe}`),
        sourceLinks(m),
        versionLine(m),
        calibrationLine(m)),
      h("td", {}, caps(m)),
      h("td", { class: "num" }, contextCell(m)),
      h("td", {}, installBadge(m)),
      h("td", {}, stateBadge(m), stateDetail(m)),
      h("td", { class: "num" }, m.kind === "remote" ? "–" : gb(m.mem_gb)),
      h("td", {}, actions(m)))));
}

// ---------- drawer: worker log / install output ----------

let drawerModel = null;
let drawerView = "logs";

function openDrawer(name, view) {
  const m = models.find((x) => x.name === name);
  if (m && m.kind === "remote") return;
  drawerModel = name;
  drawerView = view;
  $("#drawer").hidden = false;
  for (const b of document.querySelectorAll(".drawer-tabs .seg")) b.setAttribute("aria-pressed", b.dataset.view === view);
  $("#drawer-log").textContent = "";
  renderModels();
  refreshDrawer(true);
}
$("#drawer-close").addEventListener("click", () => { drawerModel = null; $("#drawer").hidden = true; renderModels(); });
for (const b of document.querySelectorAll(".drawer-tabs .seg")) b.addEventListener("click", () => openDrawer(drawerModel, b.dataset.view));

async function refreshDrawer(scrollToEnd = false) {
  const name = drawerModel;
  if (!name) return;
  const m = models.find((x) => x.name === name);
  $("#drawer-title").textContent = name;
  $("#drawer-sub").textContent = m ? `${m.recipe} · ${m.installed} · ${m.state}` : "";
  $("#drawer-finder").onclick = () => showInFinder("log", name);
  const pre = $("#drawer-log");
  const atEnd = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
  let lines = [];
  try {
    if (drawerView === "job") {
      const j = await api("GET", `/admin/models/${encodeURIComponent(name)}/job?n=500`);
      lines = j.lines || [];
      if (j.state !== "running") lines.push("", `[${j.kind} ${j.state}${j.finished ? " " + ago(j.finished) : ""}]`);
    } else {
      const l = await api("GET", `/admin/models/${encodeURIComponent(name)}/logs?n=400`);
      lines = l.lines || [];
      if (!lines.length) lines = ["(no worker output yet: the model hasn't been started)"];
    }
  } catch (e) {
    lines = [e.status === 404 && drawerView === "job" ? "(no install has run for this model since the server started)" : e.message];
  }
  if (drawerModel !== name) return;
  const text = lines.join("\n");
  if (pre.textContent !== text) {
    pre.textContent = text;
    if (atEnd || scrollToEnd) pre.scrollTop = pre.scrollHeight;
  }
}

// ---------- playground ----------

const presets = [
  {
    name: "Support ticket",
    state: "Help! My payouts have been failing for 3 days and I was charged twice.",
    questions: {
      is_urgent: { type: "noul", instructions: "Does this convey urgency?", criteria: { true: "Explicitly time-sensitive", false: "No urgency expressed" } },
      department: { type: "choice", instructions: "Which team should handle this?", criteria: { billing: "Payments, invoicing, refunds", technical: "Bugs, outages, integrations", sales: "Pricing, upgrades, new accounts" } },
      frustration: { type: "score", instructions: "How frustrated is the customer?", criteria: ["Calm", "Frustrated", "Very angry"] },
    },
  },
  {
    name: "Policy check",
    state: { policy: "Reports received by the deadline are on time. Reports received within 14 days after the deadline are late but accepted. Later reports are refused.", case: "The deadline was 1 March. The report arrived on 20 March." },
    questions: {
      status: { type: "score", instructions: "How late is this report?", criteria: ["On time", "Late but accepted", "Refused"] },
      accepted: { type: "noul", instructions: "Is the report accepted?" },
    },
  },
  {
    name: "Multiple choice",
    state: { question: "Which gas makes up most of Earth's atmosphere?" },
    questions: {
      answer: { type: "choice", instructions: "Which option correctly answers the question?", criteria: { a: "Oxygen", b: "Nitrogen", c: "Carbon dioxide", d: "Argon" } },
    },
  },
];

const playSelected = new Set();
let playLoaded = false;

function loadPreset(i) {
  const p = presets[i];
  $("#play-state").value = typeof p.state === "string" ? p.state : JSON.stringify(p.state, null, 2);
  $("#play-questions").value = JSON.stringify(p.questions, null, 2);
}

function initPlayground() {
  const sel = $("#play-preset");
  sel.replaceChildren(h("option", { value: "" }, "Load an example…"), ...presets.map((p, i) => h("option", { value: i }, p.name)));
  sel.addEventListener("change", () => { if (sel.value !== "") loadPreset(+sel.value); sel.value = ""; });
  loadPreset(0);
  $("#play-send").addEventListener("click", sendPlayground);
}

function playable(m) { return m.kind === "remote" || m.installed === "installed"; }

function renderPlayModels() {
  if (!playLoaded && models.length) {
    for (const m of models) if (m.state === "ready") playSelected.add(m.name);
    playLoaded = true;
  }
  $("#play-models").replaceChildren(...models.map((m) => {
    const ok = playable(m);
    const on = ok && playSelected.has(m.name);
    return h("label", {
      class: "chip" + (on ? " on" : "") + (ok ? "" : " disabled"),
      title: ok ? (m.state === "ready" ? "loaded" : m.kind === "remote" ? "hosted" : "starts on first request") : "not installed",
      onclick: (ev) => {
        ev.preventDefault();
        if (!ok) return;
        playSelected.has(m.name) ? playSelected.delete(m.name) : playSelected.add(m.name);
        renderPlayModels();
      },
    }, m.name);
  }));
}

function tryInPlayground(name) {
  playSelected.clear();
  playSelected.add(name);
  playLoaded = true;
  showTab("playground");
}

async function sendPlayground() {
  const err = $("#play-error");
  err.hidden = true;
  let state, questions;
  const raw = $("#play-state").value.trim();
  try { state = raw.startsWith("{") || raw.startsWith("[") ? JSON.parse(raw) : raw; } catch (e) { err.textContent = "State looks like JSON but doesn't parse: " + e.message; err.hidden = false; return; }
  try { questions = JSON.parse($("#play-questions").value); } catch (e) { err.textContent = "Questions aren't valid JSON: " + e.message; err.hidden = false; return; }
  const chosen = models.filter((m) => playSelected.has(m.name) && playable(m));
  if (!chosen.length) { err.textContent = "Pick at least one installed model."; err.hidden = false; return; }

  const results = $("#play-results");
  const cards = new Map();
  results.replaceChildren(...chosen.map((m) => {
    const c = h("div", { class: "card result" },
      h("div", { class: "result-head" }, h("h2", {}, m.name), h("span", { class: "lat" }, m.state === "ready" || m.kind === "remote" ? "…" : "starting…")));
    cards.set(m.name, c);
    return c;
  }));
  const strict = $("#play-strict").checked;
  const btn = $("#play-send");
  btn.disabled = true;
  await Promise.all(chosen.map(async (m) => {
    const t0 = performance.now();
    const card = cards.get(m.name);
    try {
      const resp = await fetch("/v1/systemone", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ model: m.name, state, questions, x_strict: strict }),
      });
      const data = await resp.json();
      const ms = Math.round(performance.now() - t0);
      card.replaceChildren(...renderResult(m.name, resp, data, ms, questions, m.kind !== "remote" && m.state !== "ready").filter(Boolean));
    } catch (e) {
      card.replaceChildren(h("div", { class: "result-head" }, h("h2", {}, m.name)), h("div", { class: "error-text" }, e.message));
    }
  }));
  btn.disabled = false;
  refresh();
}

function renderResult(name, resp, data, ms, questions, cold) {
  const serverMs = resp.headers.get("X-Cyprid-Latency-Ms");
  const head = h("div", { class: "result-head" }, h("h2", {}, name),
    h("span", { class: "lat", title: cold ? "includes starting the model" : "time in cyprid and the backend" }, `${serverMs ?? ms} ms${cold ? " (incl. start)" : ""}`));
  if (!resp.ok) {
    return [head, h("div", { class: "error-text" }, `${resp.status}: ${typeof data.detail === "string" ? data.detail : JSON.stringify(data.detail)}`),
      data.logs ? h("pre", { class: "log", style: { height: "160px", marginTop: "8px", borderRadius: "7px" } }, data.logs.join("\n")) : null];
  }
  const qs = Object.entries(data.answers || {}).map(([qid, a]) => renderAnswer(qid, a, questions[qid]));
  const norm = resp.headers.get("X-Cyprid-Normalised");
  const normNote = norm ? h("div", { class: "muted small", style: { marginBottom: "8px" },
    title: "This backend's response differed from Jev's format; cyprid reshaped it without changing any probability." },
    "Reshaped to Jev's format: " + norm) : null;
  return [head, normNote, ...qs,
    h("details", { class: "raw-toggle" }, h("summary", {}, "Raw response"), h("pre", {}, JSON.stringify(data, null, 2)))];
}

function bars(entries, winner) {
  return entries.map(([label, p]) =>
    h("div", { class: "opt" + (label === winner ? " win" : "") },
      h("span", { class: "label", title: label }, label),
      h("span", { class: "track" }, h("span", { class: "fill", style: { width: Math.max(0, Math.min(1, p)) * 100 + "%", display: "block" } })),
      h("span", { class: "pct" }, pct(p))));
}

function renderAnswer(qid, a, q) {
  const conf = a.confidence != null ? `confidence ${pct(a.confidence)}` : "";
  const emulated = a.x_emulated ? h("span", { class: "badge warn", title: "This model doesn't support this question type natively; the answer is an approximation" }, "emulated") : null;
  const truncated = a.x_truncated ? h("span", { class: "badge warn", title: "The backend reported probabilities for only some options; the rest are shown as 0" }, "partial") : null;
  const title = h("div", { class: "q-title" }, h("span", {}, h("span", { class: "qid" }, qid), " ", h("span", { class: "muted" }, a.type), " ", emulated, " ", truncated), h("span", { class: "qconf" }, conf));
  let body = [];
  if (a.type === "noul") {
    body = bars([["yes", a.noul], ["no", 1 - a.noul]], a.noul >= 0.5 ? "yes" : "no");
  } else if (a.type === "choice") {
    const order = q && q.criteria && typeof q.criteria === "object" ? Object.keys(q.criteria) : Object.keys(a.probabilities || {});
    body = bars(order.map((k) => [k, (a.probabilities || {})[k] ?? 0]), a.choice);
  } else if (a.type === "score") {
    const legend = a.legend || {};
    const keys = Object.keys(a.probabilities || {}).sort((x, y) => +x - +y);
    const winner = keys.reduce((best, k) => ((a.probabilities[k] > (a.probabilities[best] ?? -1)) ? k : best), keys[0]);
    body = bars(keys.map((k) => [`${k}: ${legend[k] ?? ""}`, a.probabilities[k]]), `${winner}: ${legend[winner] ?? ""}`);
    body.push(h("div", { class: "score-line" }, `score ${Number(a.score).toFixed(2)} of ${keys.length - 1}`));
  } else {
    body = [h("pre", {}, JSON.stringify(a, null, 2))];
  }
  return h("div", { class: "q" }, title, body);
}

// ---------- recipes ----------

let recipeSelected = null;

async function loadRecipes() {
  try {
    const { recipes } = await api("GET", "/admin/recipes");
    $("#recipe-list").replaceChildren(...recipes.map((r) =>
      h("li", { class: r.id === recipeSelected ? "selected" : "", onclick: () => openRecipe(r.id) },
        h("span", {}, h("div", {}, r.id), h("div", { class: "muted small" }, r.models.join(", "))),
        h("span", { class: "badge " + (r.source === "user" ? "warn" : "") }, r.source === "user" ? (r.has_builtin ? "edited" : "custom") : "built-in"))));
  } catch (e) { toast(e.message, true); }
}

async function openRecipe(id) {
  recipeSelected = id;
  loadRecipes();
  const ed = $("#recipe-editor");
  let r;
  try { r = await api("GET", `/admin/recipes/${encodeURIComponent(id)}`); } catch (e) { toast(e.message, true); return; }
  const area = h("textarea", { spellcheck: "false" });
  area.value = r.yaml;
  area.addEventListener("keydown", (ev) => {
    if (ev.key === "Tab") { ev.preventDefault(); const s = area.selectionStart; area.setRangeText("  ", s, area.selectionEnd, "end"); }
    if ((ev.metaKey || ev.ctrlKey) && ev.key === "s") { ev.preventDefault(); save(); }
  });
  const notice = h("div", { class: "notice", hidden: true });
  const show = (msg, ok) => { notice.textContent = msg; notice.className = "notice " + (ok ? "ok" : "bad"); notice.hidden = false; };

  async function save() {
    try {
      const res = await api("PUT", `/admin/recipes/${encodeURIComponent(id)}`, { yaml: area.value });
      show(`Saved to ${res.saved}. It applies the next time a model starts.` + (res.outdated && res.outdated.length ? ` Reinstall needed (install section changed): ${res.outdated.join(", ")}.` : ""), true);
      loadRecipes(); refresh();
    } catch (e) { show(e.message, false); }
  }
  async function reset() {
    const what = r.has_builtin ? "Discard your changes and go back to the built-in recipe?" : "Delete this custom recipe?";
    if (!confirm(what)) return;
    try {
      await api("DELETE", `/admin/recipes/${encodeURIComponent(id)}`);
      toast(r.has_builtin ? `${id} reset to built-in` : `${id} deleted`);
      refresh();
      if (r.has_builtin) openRecipe(id); else { recipeSelected = null; ed.replaceChildren(h("div", { class: "empty" }, "Select a recipe.")); loadRecipes(); }
    } catch (e) { show(e.message, false); }
  }

  ed.replaceChildren(
    h("div", { class: "row-between" },
      h("div", {}, h("h2", {}, id), h("div", { class: "muted small" }, r.source === "user" ? `Your version: ${r.path}` : "Built-in. Saving creates your own copy.")),
    ),
    area, notice,
    h("div", { class: "editor-actions" },
      h("button", { class: "btn primary", onclick: save }, "Save"),
      h("button", { class: "btn", onclick: () => { area.value = r.yaml; notice.hidden = true; } }, "Revert unsaved changes"),
      h("span", { class: "spacer" }),
      r.source === "user" ? finderButton("recipe", id) : null,
      r.source === "user" ? h("button", { class: "btn danger", onclick: reset }, r.has_builtin ? "Reset to built-in" : "Delete recipe") : null));
}

$("#recipe-new").addEventListener("click", async () => {
  const id = prompt("New recipe id (letters, digits, - _ .):");
  if (!id) return;
  const yaml = `id: ${id}
description: A Jev-compatible server
# For an already-running server, use remote instead of install/launch:
# remote:
#   url: http://127.0.0.1:9000
install:
  python: "3.12"
  packages: []
launch:
  cmd: ["{python}", "-m", "your_server", "--port", "{port}"]
  ready:
    path: /health
    timeout: 5m
  idle_timeout: 30m
  mem_gb: 2
models:
  ${id}:
    description: ""
`;
  try {
    await api("PUT", `/admin/recipes/${encodeURIComponent(id)}`, { yaml });
    await loadRecipes();
    openRecipe(id);
    refresh();
  } catch (e) { toast(e.message, true); }
});

// ---------- benchmarks ----------

let benchSelected = null;

async function loadBench() {
  let runs;
  try { ({ runs } = await api("GET", "/admin/bench")); } catch (e) { toast(e.message, true); return; }
  runs = runs || [];
  $("#bench-list").replaceChildren(...runs.map((r) =>
    h("li", { class: r.id === benchSelected ? "selected" : "", onclick: () => openBench(r.id) },
      h("span", { class: "run-id" }, r.id, r.partial ? h("span", { class: "badge warn", style: { marginLeft: "6px" } }, "partial") : null),
      h("span", { class: "muted small" }, `${r.suite} · ${new Date(r.started_at).toLocaleString()}`),
      h("span", { class: "small" }, r.models.map((m) => `${m.model} ${pct(m.acc)}`).join(" · ")))));
  if (!benchSelected && runs.length) openBench(runs[0].id);
}

function heat(v) {
  // 0.25 (chance-ish) -> bad, 1.0 -> ok; mixes the theme's colours.
  const t = Math.max(0, Math.min(1, (v - 0.25) / 0.75));
  const pctOk = Math.round(t * 100);
  return { background: `color-mix(in srgb, var(--ok-soft) ${pctOk}%, var(--bad-soft))` };
}

async function openBench(id) {
  benchSelected = id;
  loadBench();
  let rep;
  try { rep = await api("GET", `/admin/bench/${encodeURIComponent(id)}`); } catch (e) { toast(e.message, true); return; }
  const ms = rep.models || [];
  const pub = Object.entries(rep.published || {});
  const cols = [...ms.map((m) => ({ name: m.model })), ...pub.map(([n]) => ({ name: n, pub: true }))];
  const colHead = (c) => h("th", { class: c.pub ? "pub" : "" }, c.name, c.pub ? h("div", { class: "sub" }, "published") : null);

  const f3 = (v) => v.toFixed(3);
  const metricRows = [
    ["accuracy", (m) => m.clean.acc, (p) => p.Acc, "high", pct],
    ["Brier (lower is better)", (m) => m.clean.brier, (p) => p.Brier || null, "low", f3],
    ["ECE (lower is better)", (m) => m.clean.ece, null, "low", f3],
    ["answered at ≥ 0.9", (m) => m.clean.coverage_at_0_9, null, null, pct],
    ["accuracy of those", (m) => m.clean.acc_at_0_9 ?? null, null, "high", pct],
    ["confident errors", (m) => m.clean.confident_error_rate, null, "low", pct],
    ["latency p50 (ms)", (m) => Math.round(m.latency_ms.P50), null, "low", (v) => v],
    ["latency p95 (ms)", (m) => Math.round(m.latency_ms.P95), null, "low", (v) => v],
    ["questions scored", (m) => m.clean.n, null, null, (v) => v],
    ["failed requests", (m) => m.clean.errors, null, null, (v) => v],
  ];
  const headTable = h("table", { class: "metrics" },
    h("thead", {}, h("tr", {}, h("th", {}, ""), cols.map(colHead))),
    h("tbody", {}, metricRows.map(([label, f, g, better, fmt]) => {
      const vals = ms.map(f);
      const nums = vals.filter((v) => typeof v === "number");
      const best = better && nums.length > 1 ? (better === "high" ? Math.max(...nums) : Math.min(...nums)) : null;
      return h("tr", {}, h("td", {}, label),
        vals.map((v) => h("td", { class: best != null && v === best ? "best" : "" }, typeof v === "number" ? fmt(v) : "–")),
        pub.map(([, p]) => { const v = g ? g(p) : null; return h("td", { class: "pub" }, v == null ? "–" : fmt(v)); }));
    })));

  const tierNames = ["easy", "standard", "judge", "hard"].filter((t) => ms.some((m) => (m.by_tier || {})[t]));
  const tierTable = tierNames.length ? h("table", { class: "metrics" },
    h("thead", {}, h("tr", {}, h("th", {}, "tier"), ms.map((m) => h("th", {}, m.model)))),
    h("tbody", {}, tierNames.map((t) => h("tr", {}, h("td", {}, `${t} (n=${((ms[0].by_tier || {})[t] || {}).n ?? "?"})`),
      ms.map((m) => { const v = ((m.by_tier || {})[t] || {}).acc; return h("td", {}, v == null ? "–" : h("span", { class: "heat", style: heat(v) }, pct(v))); }))))) : null;
  const hasVariants = ms.some((m) => { const r = m.robustness || {}; return (r.permuted_n || 0) + (r.none_present_n || 0) + (r.none_absent_n || 0) > 0; });
  const taskPub = pub.filter(([, p]) => p.Tasks && Object.keys(p.Tasks).length);
  const types = ["noul", "choice", "score"];
  const typeTable = h("table", { class: "metrics" },
    h("thead", {}, h("tr", {}, h("th", {}, ""), ms.map((m) => h("th", {}, m.model)))),
    h("tbody", {}, types.map((t) => h("tr", {}, h("td", {}, t),
      ms.map((m) => {
        const mt = (m.by_type || {})[t];
        if (!mt) return h("td", {}, "–");
        const em = (m.emulated_types || []).includes(t);
        return h("td", {}, h("span", { class: "heat", style: heat(mt.acc) }, pct(mt.acc)), em ? h("span", { class: "muted small" }, " emulated") : null);
      })))));

  const tasks = [...new Set(ms.flatMap((m) => Object.keys(m.by_task || {})))].sort();
  const taskCols = [...ms.map((m) => ({ name: m.model })), ...taskPub.map(([n]) => ({ name: n, pub: true }))];
  const taskTable = h("table", { class: "metrics" },
    h("thead", {}, h("tr", {}, h("th", {}, "task"), taskCols.map(colHead))),
    h("tbody", {}, tasks.map((t) => h("tr", {}, h("td", {}, `${t} (n=${(ms[0].by_task[t] || {}).n ?? "?"})`),
      ms.map((m) => { const v = (m.by_task[t] || {}).acc; return h("td", {}, v == null ? "–" : h("span", { class: "heat", style: heat(v) }, pct(v))); }),
      taskPub.map(([, p]) => { const v = (p.Tasks || {})[t]; return h("td", { class: "pub" }, v == null ? "–" : pct(v)); })))));

  const rob = h("table", { class: "metrics" },
    h("thead", {}, h("tr", {}, h("th", {}, ""), ms.map((m) => h("th", {}, m.model)))),
    h("tbody", {}, [
      ["answer flips when options are reordered", (r) => r.permuted_flip_rate],
      ["still right with “none of these” added", (r) => r.none_present_acc],
      ["picks “none” when the answer is removed", (r) => r.none_absent_acc],
    ].map(([label, f]) => h("tr", {}, h("td", {}, label), ms.map((m) => h("td", {}, pct(f(m.robustness || {}))))))));

  $("#bench-detail").replaceChildren(...[
    h("div", { class: "card" },
      h("div", { class: "row-between" }, h("h3", {}, `${rep.suite} · ${rep.records} records · ${new Date(rep.started_at).toLocaleString()}`), finderButton("run", id)),
      rep.partial ? h("p", { class: "notice bad", style: { fontFamily: "inherit" } }, "Partial run: it was interrupted, so some models answered only some records (see \"questions scored\"). Resume it with cyprid bench … --resume.") : null,
      headTable,
      pub.length ? h("p", { class: "muted small" }, pub.map(([n, p]) => `${n}: ${p.Source}${p.Note ? ` (${p.Note})` : ""}`).join(" · ")) : null),
    tierTable ? h("div", { class: "card" }, h("h3", {}, "Accuracy by tier"), tierTable) : null,
    h("div", { class: "card" }, h("h3", {}, "Accuracy by question type"), typeTable),
    h("div", { class: "card" }, h("h3", {}, "Accuracy by task"), taskTable),
    hasVariants ? h("div", { class: "card" }, h("h3", {}, "Robustness"), rob) : null].filter(Boolean));
}

// ---------- network access ----------

let netInfo = null;
let keyShown = false;

async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast("Copied"); } catch { toast("Couldn't copy; select the text instead", true); }
}

function renderNetPill() {
  const on = !!status.network;
  $("#net-pill").classList.toggle("on", on);
  $("#net-pill-text").textContent = on ? "On network" : "Local only";
}

function renderNetDialog() {
  const n = netInfo;
  if (!n) return;
  $("#net-toggle").checked = n.enabled;
  $("#net-details").hidden = !n.enabled;
  const err = $("#net-error");
  err.hidden = !(n.enabled && !n.listening);
  err.textContent = n.error ? `Network mode is on but not listening: ${n.error}` : "Network mode is on but not listening.";
  $("#net-urls").replaceChildren(...(n.urls || []).map((u) =>
    h("li", {}, h("span", {}, u), h("button", { class: "btn small", onclick: () => copy(u) }, "Copy"))));
  $("#net-key").textContent = keyShown ? n.api_key || "" : "•".repeat(24);
  $("#net-show").textContent = keyShown ? "Hide" : "Show";
  const base = (n.urls || [])[1] || (n.urls || [])[0] || `http://<this-mac>:${n.port}`;
  $("#net-example").textContent =
`curl ${base}/v1/systemone \\
  -H "Authorization: Bearer ${keyShown ? n.api_key : "<API key>"}" \\
  -d '{"model": "laya", "state": "I was charged twice", "questions": {"billing": {"type": "noul", "instructions": "Is this about billing?"}}}'`;
}

async function openNetDialog() {
  try { netInfo = await api("GET", "/admin/network"); } catch (e) { toast(e.message, true); return; }
  keyShown = false;
  renderNetDialog();
  $("#net-dialog").showModal();
}

$("#net-pill").addEventListener("click", openNetDialog);
$("#net-close").addEventListener("click", () => $("#net-dialog").close());
$("#net-dialog").addEventListener("click", (ev) => { if (ev.target === ev.currentTarget) ev.currentTarget.close(); });
$("#net-toggle").addEventListener("change", async (ev) => {
  const want = ev.target.checked;
  ev.target.disabled = true;
  try {
    netInfo = await api("PUT", "/admin/network", { enabled: want });
    toast(want ? "Listening on the network" : "Local only: network connections refused");
  } catch (e) {
    toast(e.message, true);
    try { netInfo = await api("GET", "/admin/network"); } catch { /* keep old */ }
  } finally {
    ev.target.disabled = false;
    renderNetDialog();
    refresh();
  }
});
$("#net-show").addEventListener("click", () => { keyShown = !keyShown; renderNetDialog(); });
$("#net-copy").addEventListener("click", () => netInfo && copy(netInfo.api_key));
$("#net-rotate").addEventListener("click", async () => {
  if (!confirm("Make a new API key? Machines using the current key will stop working until they switch.")) return;
  try { netInfo = await api("POST", "/admin/network/rotate-key"); keyShown = true; renderNetDialog(); toast("New API key created"); }
  catch (e) { toast(e.message, true); }
});

for (const b of document.querySelectorAll("[data-reveal]")) b.addEventListener("click", () => showInFinder(b.dataset.reveal));

// ---------- boot ----------

initPlayground();
await refresh();
const initial = location.hash.slice(1);
showTab(["models", "playground", "recipes", "bench"].includes(initial) ? initial : "models");
setInterval(() => { if (!document.hidden && (currentTab === "models" || drawerModel)) refresh(); }, 1500);
setInterval(() => { if (!document.hidden && currentTab === "playground") renderPlayModels(); }, 3000);
