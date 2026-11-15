"use strict";

// The CLI opens http://127.0.0.1:<port>/#token=<token>. Keep the token for
// this tab and take it out of the address bar.
// Re-running `chainlink ui` on the same port only changes the fragment, which
// doesn't reload the page, so a new token is also picked up on hashchange.
const TOKEN_KEY = "chainlink-ui-token";
let token = "";

function readToken() {
  const m = /(?:^|[#&])token=([0-9a-f]+)/.exec(location.hash);
  if (m) {
    token = m[1];
    try { sessionStorage.setItem(TOKEN_KEY, token); } catch (_) {}
    history.replaceState(null, "", location.pathname);
  } else if (!token) {
    try { token = sessionStorage.getItem(TOKEN_KEY) || ""; } catch (_) {}
  }
}
readToken();
window.addEventListener("hashchange", () => { readToken(); load(); });

const $ = (id) => document.getElementById(id);

// h builds an element; strings become text nodes, never HTML.
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

// setChildren is replaceChildren that flattens arrays and drops null/false.
function setChildren(el, ...children) {
  el.replaceChildren(...children.flat().filter((c) => c != null && c !== false));
}

function dot(state) { return h("span", { class: "dot " + state }); }

function banner(text) {
  const b = $("banner");
  b.hidden = !text;
  b.textContent = text || "";
}

let toastTimer = 0;
function toast(text) {
  const t = $("toast");
  t.textContent = text;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 6000);
}

// ---------- API ----------

async function api(path, opts = {}) {
  const res = await fetch(path, {
    ...opts,
    cache: "no-store",
    headers: { "X-ChainLink-UI-Token": token, ...(opts.body ? { "Content-Type": "application/json" } : {}) },
  });
  if (!res.ok) {
    const err = new Error((await res.text()).trim() || "HTTP " + res.status);
    err.status = res.status;
    throw err;
  }
  return res.json();
}

// ---------- Running commands ----------

let status = null; // last /api/status
const outputs = new Map(); // job id -> { lines: [], next: 0, view }
let selected = 0; // job shown in the drawer
let polling = 0; // job being polled

function runningJob(action) {
  return status && status.jobs.find((j) => j.running && j.action === action);
}

// busy is the running foreground job, if any; only serve runs alongside others.
function busy() {
  return status && status.jobs.find((j) => j.running && j.action !== "serve");
}

async function run(action, { input, confirmText } = {}) {
  if (confirmText && !confirm(confirmText)) return false;
  try {
    const job = await api("api/jobs", { method: "POST", body: JSON.stringify({ action, input }) });
    status.jobs.unshift(job);
    showJob(job.id);
    load();
    // serve takes a moment to start listening; show it running without waiting for the next refresh.
    if (action === "serve") setTimeout(load, 1000);
    return true;
  } catch (e) {
    toast(e.status === 401 ? "This link has expired. Open the one `chainlink ui` printed." : e.message);
    return false;
  }
}

async function stopJob(id) {
  try { await api("api/jobs/" + id + "/stop", { method: "POST", body: "{}" }); } catch (e) { toast(e.message); }
  load();
}

// runButton runs action on click. The command it runs is shown beside it.
function runButton(label, action, opts = {}) {
  const mine = runningJob(action);
  const blocked = !mine && action !== "serve" && busy();
  const btn = h("button", {
    type: "button",
    class: "run" + (opts.primary === false ? "" : " primary"),
    disabled: !!(mine || blocked),
    title: blocked ? "Wait for " + blocked.command + " to finish" : null,
    onclick: () => run(action, opts),
  }, mine ? "Running…" : label);
  return h("span", { class: "action" }, btn, h("code", { class: "muted small" }, opts.command || "chainlink " + action));
}

// ---------- Output drawer ----------

function showJob(id) {
  selected = id;
  $("drawer").hidden = false;
  document.body.classList.add("drawer-open");
  renderDrawer();
  poll(id);
}

function hideDrawer() {
  $("drawer").hidden = true;
  document.body.classList.remove("drawer-open");
}

async function poll(id) {
  if (polling === id) return;
  polling = id;
  try {
    for (;;) {
      const out = outputs.get(id) || { lines: [], next: 0, view: null };
      const v = await api("api/jobs/" + id + "?from=" + out.next);
      if (v.from > out.next) out.lines.push("… (" + (v.from - out.next) + " earlier lines dropped)");
      out.lines.push(...(v.lines || []));
      out.next = v.next;
      out.view = v;
      outputs.set(id, out);
      if (selected === id) renderOutput(out);
      if (!v.running) { load(); break; }
      if (selected !== id || $("drawer").hidden) break;
      await new Promise((r) => setTimeout(r, 400));
    }
  } catch (_) {
    // The status poll reports a lost connection.
  } finally {
    if (polling === id) polling = 0;
  }
}

function jobState(v) {
  if (!v) return ["", "…"];
  if (v.running) return ["warn", "Running"];
  if (v.stopped) return ["", "Stopped"];
  if (v.error) return ["bad", v.error];
  return v.exitCode === 0 ? ["ok", "Done"] : ["bad", "Failed (exit " + v.exitCode + ")"];
}

function renderDrawer() {
  if (!status) return;
  const jobs = status.jobs.slice(0, 8);
  setChildren($("job-tabs"), jobs.map((j) => {
    const [state] = jobState(j);
    return h("button", {
      type: "button",
      class: "tab" + (j.id === selected ? " active" : ""),
      "aria-pressed": j.id === selected ? "true" : "false",
      onclick: () => showJob(j.id),
    }, dot(state), j.action);
  }));
  const out = outputs.get(selected);
  renderOutput(out || { lines: [], view: jobs.find((j) => j.id === selected) });
}

function renderOutput(out) {
  const v = out.view;
  const [state, text] = jobState(v);
  setChildren($("job-status"), v ? [h("code", {}, v.command), " ", dot(state), " ", text] : []);
  const stop = $("job-stop");
  stop.hidden = !(v && v.running);
  stop.onclick = v ? () => stopJob(v.id) : null;

  const pre = $("output");
  const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24;
  pre.textContent = out.lines.length ? out.lines.join("\n") : (v && v.running ? "Starting…" : "(no output)");
  if (atBottom) pre.scrollTop = pre.scrollHeight;
}

// ---------- Setup ----------

function keyForm() {
  const input = h("input", {
    type: "password", id: "cloud-key", autocomplete: "off", spellcheck: "false",
    placeholder: "Paste an Open Cloud API key", "aria-label": "Open Cloud API key",
  });
  const form = h("form", { class: "key-form" }, input,
    h("button", { type: "submit", class: "run primary", disabled: !!busy() }, "Save"));
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const key = input.value.trim();
    if (!key) { input.focus(); return; }
    if (await run("login-cloud", { input: key })) input.value = "";
  });
  return form;
}

function renderSetup(s) {
  const rows = [
    {
      title: "ChainLink installed",
      state: s.binInstalled ? "ok" : "bad",
      detail: s.binDir,
      fix: s.binInstalled ? null : runButton("Install", "self-install"),
    },
    {
      title: "On your PATH",
      state: s.onPath ? "ok" : s.pathPending ? "warn" : "bad",
      detail: s.onPath ? "chainlink runs from any terminal"
        : s.pathPending ? "Added to your shell config. Open a new terminal to pick it up."
        : "Add " + s.binDir + " to PATH",
      fix: s.onPath || s.pathPending ? null : runButton("Add to PATH", "self-install"),
    },
    {
      title: "Roblox Studio plugin",
      state: s.pluginInstalled ? "ok" : "bad",
      detail: s.pluginPath || "Unsupported platform",
      fix: s.pluginInstalled ? null : runButton("Install plugin", "self-install"),
    },
    {
      title: "Open Cloud API key",
      state: s.cloudKey ? "ok" : "warn",
      detail: s.cloudKey ? "Saved. Used by assets and publish-place."
        : "Optional. Needed to upload assets and publish places. Create one at create.roblox.com/credentials with Assets read and write.",
      fix: s.cloudKey ? null : keyForm(),
    },
  ];
  setChildren($("setup"), rows.map((r) => h("li", { class: "check" },
    dot(r.state),
    h("div", { class: "check-body" },
      h("div", { class: "check-title" }, r.title),
      h("div", { class: "check-detail muted small" }, r.detail)),
    h("div", { class: "check-fix" }, r.fix))));
}

// ---------- Project ----------

function tile(label, value, state, action) {
  return h("div", { class: "tile" },
    h("div", { class: "tile-label" }, label),
    h("div", { class: "tile-value" }, state ? dot(state) : null, value),
    action);
}

function depsTable(title, deps, kindWord) {
  const rows = deps.map((d) => h("tr", {},
    h("td", {}, d.alias),
    h("td", { class: "spec" }, d.spec),
    h("td", { class: "state" },
      d.error ? [dot("bad"), "Invalid"]
        : d.installed ? [dot("ok"), "Installed"]
        : [dot("warn"), "Not installed"])));
  return [
    h("h3", {}, title),
    deps.length
      ? h("table", {},
          h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Source"), h("th", {}, "Status"))),
          h("tbody", {}, rows))
      : h("div", { class: "empty" }, "No " + kindWord + " yet. Add one with ", h("code", {}, "chainlink add")),
  ];
}

function countInstalled(deps) {
  return deps.filter((d) => d.installed).length;
}

function syncTile(sync) {
  const mine = runningJob("serve");
  if (mine) {
    return tile("Live sync", sync.running ? "Running" : "Starting…", sync.running ? "ok" : "warn",
      h("span", { class: "action" },
        h("button", { type: "button", class: "run", onclick: () => stopJob(mine.id) }, "Stop"),
        h("button", { type: "button", class: "run", onclick: () => showJob(mine.id) }, "Log")));
  }
  if (sync.running) {
    return tile("Live sync", "Running", "ok", h("span", { class: "muted small" }, "Started outside this page"));
  }
  return tile("Live sync", "Stopped", "bad", runButton("Start", "serve"));
}

function renderProject(st) {
  const root = $("project");
  if (st.error) {
    setChildren(root, h("p", {}, dot("bad"), " ", st.error));
    return;
  }
  const p = st.project;
  if (!p) {
    setChildren(root,
      h("p", { class: "muted" }, "No chain.toml in ", h("code", {}, st.dir), "."),
      h("p", {}, runButton("Create a project here", "init", {
        confirmText: "Create a ChainLink project (chain.toml, client/, server/, shared/, …) in " + st.dir + "?",
      })),
      h("p", { class: "muted small" }, "Or run ", h("code", {}, "chainlink ui"), " from an existing project folder."));
    return;
  }

  const deps = p.tools.length + p.packages.length;
  const installed = countInstalled(p.tools) + countInstalled(p.packages);
  const missing = deps - installed;

  const r = p.roblox || {};
  const creator = r.GroupId ? "group " + r.GroupId : r.UserId ? "user " + r.UserId : null;
  const a = p.assets;

  let assetTile;
  if (!a) {
    assetTile = tile("Assets", "No assets/", null, null);
  } else if (a.pending) {
    assetTile = tile("Assets", a.pending + " to upload", "warn",
      !st.setup.cloudKey ? h("span", { class: "muted small" }, "Save an Open Cloud key first")
        : !creator ? h("span", { class: "muted small" }, "Set [Roblox] UserId or GroupId")
        : runButton("Upload", "assets", {
            confirmText: "Upload " + a.pending + " new or changed file(s) from assets/ to Roblox, owned by " + creator + "?",
          }));
  } else {
    assetTile = tile("Assets", a.total ? a.uploaded + " uploaded" : "None yet", a.total ? "ok" : null, null);
  }

  setChildren(root,
    h("div", { class: "project-head" },
      h("span", { class: "project-name" }, p.name || "(unnamed)"),
      p.version ? h("span", { class: "muted" }, "v" + p.version) : null),
    p.description ? h("p", { class: "project-desc" }, p.description) : null,
    h("p", { class: "project-dir muted small" }, st.dir),

    h("div", { class: "tiles" },
      syncTile(p.sync),
      missing
        ? tile("Dependencies", missing + " missing", "warn", runButton("Install", "install", { command: "chainlink install -y" }))
        : tile("Dependencies", deps ? "All " + deps + " installed" : "None", deps ? "ok" : null, null),
      assetTile,
      tile("Place", r.PlaceId ? String(r.PlaceId) : "Not set", r.PlaceId ? "ok" : null, null)),

    p.sync.running ? null : h("p", { class: "muted small" },
      "The Studio plugin syncs from port " + p.sync.port + " while live sync is running."),

    depsTable("Tools", p.tools, "tools"),
    depsTable("Packages", p.packages, "packages"),

    h("h3", {}, "Roblox"),
    h("dl", { class: "kv" },
      h("dt", {}, "Place ID"), h("dd", {}, r.PlaceId ? String(r.PlaceId) : "Not set ([Roblox] PlaceId in chain.toml)"),
      h("dt", {}, "Asset owner"), h("dd", {}, creator ? creator[0].toUpperCase() + creator.slice(1) : "Not set ([Roblox] UserId or GroupId)"),
      a ? [h("dt", {}, "Asset files"), h("dd", {},
        a.total + " file(s), " + a.uploaded + " uploaded, " + a.pending + " new or changed" +
        (a.skipped ? ", " + a.skipped + " unsupported" : ""))] : null),
  );
}

// ---------- Loading ----------

// Sections are rebuilt only when what they show changed, so a half-typed API
// key or a focused button survives the background refresh.
const rendered = {};
function renderIfChanged(name, data, fn) {
  const key = JSON.stringify(data);
  if (rendered[name] === key) return;
  rendered[name] = key;
  fn();
}

let loading = false;

async function load() {
  if (loading) return;
  if (!token) {
    banner("This page needs the link printed by `chainlink ui`. Run it again and open that link.");
    return;
  }
  loading = true;
  try {
    status = await api("api/status");
    banner("");
    $("version").textContent = status.version;
    const jobsKey = status.jobs.map((j) => [j.id, j.action, j.running]);
    const up = $("update");
    up.hidden = !status.latest;
    renderIfChanged("update", [status.latest, jobsKey], () => setChildren(up,
      status.latest ? ["ChainLink " + status.latest + " is available. ", runButton("Update", "update")] : []));
    renderIfChanged("setup", [status.setup, jobsKey], () => renderSetup(status.setup));
    renderIfChanged("project", [status.project, status.error, status.dir, status.setup.cloudKey, jobsKey], () => renderProject(status));
    $("show-output").hidden = status.jobs.length === 0;
    if (!$("drawer").hidden) {
      renderDrawer();
      const sel = status.jobs.find((j) => j.id === selected);
      if (sel && sel.running) poll(sel.id);
    }
    $("updated").textContent = "Updated " + new Date().toLocaleTimeString();
  } catch (e) {
    banner(e.status === 401
      ? "This link is from an earlier `chainlink ui` run. Open the link it prints now."
      : "Lost contact with chainlink ui. Is it still running?");
  } finally {
    loading = false;
  }
}

$("refresh").addEventListener("click", load);
$("show-output").addEventListener("click", () => status && status.jobs[0] && showJob(selected || status.jobs[0].id));
$("job-close").addEventListener("click", hideDrawer);
window.addEventListener("focus", load);
setInterval(() => { if (!document.hidden) load(); }, 5000);
load();
