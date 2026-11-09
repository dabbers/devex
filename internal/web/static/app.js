// dabberz control plane, set in the Broadsheet system.
//
// Vocabulary: the interface says repository / project / sub-task where the API
// says repo / task / fork. The mapping is deliberate and one-to-one; only the
// words differ, so "project" below always means one task and "sub-task" always
// means one fork with its own machine and its own preview URL.
//
// Everything rendered here is untrusted: agent output, verifier reports and
// workstream names all flow into these views. Nothing is ever assigned to
// innerHTML; `el` builds nodes and sets text, so a name containing markup is
// displayed rather than executed.

// session holds whatever the sign-in flow produced. The server reads the
// provider's cookie, which is what protects the event stream and the shell
// (neither an EventSource nor a WebSocket handshake can carry a header). The
// bearer token is belt and braces for ordinary fetches.
const session = {
  async token() {
    try {
      return (await window.Clerk?.session?.getToken()) || null;
    } catch {
      return null;
    }
  },
};

async function authHeaders(extra) {
  const headers = { ...(extra || {}) };
  const token = await session.token();
  if (token) headers.authorization = "Bearer " + token;
  return headers;
}

const api = {
  async get(path) {
    const res = await fetch(path, { headers: await authHeaders({ accept: "application/json" }) });
    if (res.status === 401) return signInRequired();
    if (!res.ok) throw new Error(await describe(res));
    return res.json();
  },
  async send(method, path, body) {
    const res = await fetch(path, {
      method,
      headers: await authHeaders(body ? { "content-type": "application/json" } : {}),
      body: body ? JSON.stringify(body) : undefined,
    });
    if (res.status === 401) return signInRequired();
    if (!res.ok) throw new Error(await describe(res));
    return res.status === 204 ? null : res.json();
  },
};

// signInRequired stops whatever was in flight and shows the sign-in screen.
// Throwing rather than returning keeps a caller from rendering a page out of
// an empty response.
function signInRequired() {
  renderSignIn("Your session has ended.");
  throw new Error("not signed in");
}

async function describe(res) {
  try {
    const body = await res.json();
    if (body && body.error) return body.error;
  } catch {
    // Fall through to the status line.
  }
  return `${res.status} ${res.statusText}`;
}

// ---------------------------------------------------------------- dom helper

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === null || value === undefined || value === false) continue;
    if (key === "class") node.className = value;
    else if (key === "text") node.textContent = value;
    else if (key === "html") throw new Error("refusing to set untrusted html");
    else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value);
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

const clear = (node) => { while (node.firstChild) node.removeChild(node.firstChild); };

// ---------------------------------------------------------------- formatting

// One status scale across tasks and forks: quiet when nothing is needed, cyan
// while work is moving, maroon when it wants a person.
const STATE_TONE = {
  queued: "idle",
  provisioning: "busy",
  coding: "busy",
  verifying: "busy",
  fixing: "busy",
  awaiting_merge: "wait",
  merging: "busy",
  merged: "good",
  escalated: "warn",
  failed: "bad",
  abandoned: "idle",
  draft: "idle",
  planning: "busy",
  awaiting_plan: "wait",
  running: "busy",
  completed: "good",
  cancelled: "idle",
};

// What each state means in the interface's own words, rather than the
// lifecycle's. A person reading the dashboard wants to know what is happening,
// not which node of a state machine this is.
const STATE_WORDS = {
  queued: "queued",
  provisioning: "booting machine",
  coding: "working",
  verifying: "validating",
  fixing: "fixing",
  awaiting_merge: "ready to merge",
  merging: "merging",
  merged: "merged",
  escalated: "needs you",
  failed: "failed",
  abandoned: "abandoned",
  draft: "draft",
  planning: "planning",
  awaiting_plan: "awaiting approval",
  running: "running",
  completed: "complete",
  cancelled: "cancelled",
};

const stateLabel = (state) => STATE_WORDS[state] || String(state || "").replace(/_/g, " ");
const tag = (state) => el("span", { class: `tag tag-${STATE_TONE[state] || "idle"}`, text: stateLabel(state) });

function relativeTime(iso) {
  const then = new Date(iso).getTime();
  if (!Number.isFinite(then)) return "";
  const seconds = Math.round((Date.now() - then) / 1000);
  if (seconds < 45) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

const clockTime = (iso) => {
  const at = new Date(iso);
  return Number.isFinite(at.getTime())
    ? at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false })
    : "";
};

const money = (usd) => `$${(usd || 0).toFixed(2)}`;
const count = (n) => (n || 0).toLocaleString();
const plural = (n, one, many) => `${n} ${n === 1 ? one : many || one + "s"}`;

function duration(ns) {
  const seconds = Math.round((ns || 0) / 1e9);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  return `${Math.floor(minutes / 60)}h${minutes % 60 ? ` ${minutes % 60}m` : ""}`;
}

function meter(label, used, limit, format = (v) => String(v)) {
  if (!limit) return null;
  const ratio = Math.max(0, Math.min(1, used / limit));
  const tone = ratio >= 1 ? "spent" : ratio > 0.75 ? "low" : "";
  return el("span", { class: `meter ${tone}`.trim(), title: `${label}: ${format(used)} of ${format(limit)}` },
    el("span", { class: "track" }, el("span", { class: "fill", style: `width:${ratio * 100}%` })),
    `${format(used)}/${format(limit)} ${label}`);
}

function toast(message, bad = false) {
  const node = document.getElementById("toast");
  node.textContent = message;
  node.className = bad ? "toast bad" : "toast";
  node.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { node.hidden = true; }, bad ? 6000 : 3000);
}

// ---------------------------------------------------------------- live stream

const live = {
  source: null,
  cursor: 0,
  listeners: new Set(),
  retry: 1000,
  stopped: false,

  start() {
    this.stopped = false;
    this.connect();
    document.addEventListener("visibilitychange", () => {
      if (this.stopped) return;
      if (!document.hidden && (!this.source || this.source.readyState === 2)) this.connect();
    });
  },

  // stop ends the stream and keeps it ended. Without this the reconnect loop
  // keeps retrying behind the sign-in screen, where every attempt is refused,
  // producing a stream of rejected requests nobody can act on.
  stop() {
    this.stopped = true;
    if (this.source) {
      this.source.close();
      this.source = null;
    }
  },

  connect() {
    if (this.stopped) return;
    if (this.source) this.source.close();
    setConnection("connecting");

    const source = new EventSource(`/v1/events/stream?after=${this.cursor}`);
    this.source = source;

    source.onopen = () => { this.retry = 1000; setConnection("live"); };
    source.onmessage = (message) => {
      let event;
      try { event = JSON.parse(message.data); } catch { return; }
      if (event.seq > this.cursor) this.cursor = event.seq;
      for (const listener of this.listeners) listener(event);
    };
    source.onerror = () => {
      setConnection("down");
      source.close();
      if (this.stopped) return;
      setTimeout(() => this.connect(), this.retry);
      this.retry = Math.min(this.retry * 2, 15000);
    };
  },

  subscribe(listener) {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  },
};

function setConnection(state) {
  const node = document.getElementById("conn");
  node.className = `conn ${state === "live" ? "live" : state === "down" ? "down" : ""}`.trim();
  document.getElementById("conn-label").textContent =
    state === "live" ? "live"
      : state === "down" ? "reconnecting"
      : state === "signed-out" ? "signed out"
      : "connecting";
}

// ---------------------------------------------------------------- status bar

async function refreshStatus() {
  try {
    const capacity = await api.get("/v1/capacity");
    const strip = document.getElementById("status-strip");
    clear(strip);

    // Forks and workspaces both hold machines, and to the host they are the
    // same machine. Counting only forks reported none running while a
    // workspace was plainly running on one.
    const machines = capacity.machines ?? (capacity.active_forks || 0);
    strip.append(el("span", {},
      machines ? el("b", { text: count(machines) }) : "no",
      machines ? ` ${machines === 1 ? "machine" : "machines"} running` : " machines running"));

    if (capacity.queued_forks) {
      strip.append(el("span", {}, el("b", { text: count(capacity.queued_forks) }), " queued"));
    }
    const machine = capacity.machine;
    if (machine) {
      const used = machine.used || {}, total = machine.total || {};
      strip.append(el("span", {}, "vcpu ", el("b", { text: `${used.vcpus || 0}/${total.vcpus || 0}` })));
    }
    const verification = capacity.verification;
    if (verification && (verification.held || verification.waiting)) {
      strip.append(el("span", {}, "validating ", el("b", { text: `${verification.held}/${verification.capacity}` })));
    }
  } catch (err) {
    console.warn("status unavailable", err);
  }
}

// ---------------------------------------------------------------- components

function pageHead(title, { crumbs = [], lede = null, aside = null, row = [] } = {}) {
  const head = el("div", { class: "page-head" });

  if (crumbs.length) {
    const crumb = el("div", { class: "crumb" });
    crumbs.forEach((entry, index) => {
      if (index) crumb.append(" › ");
      crumb.append(entry.href ? el("a", { href: entry.href, text: entry.label }) : entry.label);
    });
    head.append(crumb);
  }

  head.append(el("div", { class: "headline-row" },
    el("div", {}, el("h1", { text: title }), lede ? el("p", { class: "lede" }, lede) : null),
    aside));

  if (row.filter(Boolean).length) head.append(el("div", { class: "row" }, ...row));
  return head;
}

const section = (title, ...body) =>
  el("section", { class: "section" }, title ? el("h6", { text: title }) : null, ...body);

const empty = (message) => el("p", { class: "empty", text: message });

// eventRow renders one audit entry.
function eventRow(event, isNew = false) {
  const where = [];
  if (event.fork_id) where.push(el("a", { href: `#/fork/${event.fork_id}`, text: "sub-task" }));
  if (event.task_id) where.push(el("a", { href: `#/task/${event.task_id}`, text: "project" }));
  if (event.repo_id) where.push(el("a", { href: `#/repo/${event.repo_id}`, text: "repository" }));

  const linked = [];
  where.forEach((node, index) => { if (index) linked.push(" · "); linked.push(node); });

  return el("div", { class: `event${isNew ? " new" : ""}` },
    el("time", { datetime: event.created_at, title: event.created_at, text: clockTime(event.created_at) }),
    el("span", { class: `actor ${event.actor === "user" ? "user" : ""}`.trim(), text: event.actor || "system" }),
    el("div", { class: "body" },
      el("div", { class: "msg" }, el("span", { class: "type", text: event.type }), event.message || ""),
      linked.length ? el("div", { class: "where" }, ...linked) : null,
      event.data && Object.keys(event.data).length
        ? el("details", { class: "data" },
            el("summary", { text: "details" }),
            el("pre", { text: JSON.stringify(event.data, null, 2) }))
        : null));
}

async function feed(path) {
  const data = await api.get(path);
  return data.events?.length
    ? el("div", {}, ...data.events.map((event) => eventRow(event)))
    : empty("Nothing recorded yet.");
}

// budgetMeters renders how close a sub-task is to its tripwire.
function budgetMeters(usage = {}, remaining = {}) {
  return [
    meter("cycles", usage.cycles || 0, (usage.cycles || 0) + (remaining.cycles || 0)),
    meter("spend", usage.cost_usd || 0, (usage.cost_usd || 0) + (remaining.cost_usd || 0), money),
  ].filter(Boolean);
}

// ---------------------------------------------------------------- views

const view = () => document.getElementById("view");

// Repositories: every repo, with the projects under it.
async function renderRepos() {
  const data = await api.get("/v1/overview");
  const root = view();
  clear(root);

  const totals = data.totals || {};
  const lede = [
    plural(totals.repos || 0, "repository", "repositories") + " onboarded",
    `${totals.running || 0} working`,
    totals.escalated ? `${totals.escalated} needs your review` : null,
  ].filter(Boolean).join(" · ");

  root.append(pageHead("Your repositories", {
    lede,
    aside: el("a", { class: "btn btn-secondary", href: "#/new", text: "New project" }),
  }));

  if (!data.repos?.length) {
    root.append(empty("No repositories yet. Add one with: dabberzctl repos add <name> <remote-url>"));
    return live.subscribe(throttle(() => { if (currentPath() === "#/") renderRepos(); }, 2500));
  }

  for (const summary of data.repos) root.append(repoBlock(summary));

  return live.subscribe(throttle(() => { if (currentPath() === "#/") renderRepos(); }, 2500));
}

function repoBlock(summary) {
  const repo = summary.repo;
  const tasks = summary.tasks || [];
  const working = summary.running + summary.queued + summary.escalated;

  // Repos with something happening open by default; quiet ones stay folded.
  const expanded = working > 0 || tasks.length > 0;

  const body = el("div", { class: "repo-body", hidden: !expanded });
  const head = el("button", {
    class: "repo-head",
    "aria-expanded": String(expanded),
    onclick: (e) => {
      const open = body.hidden;
      body.hidden = !open;
      e.currentTarget.setAttribute("aria-expanded", String(open));
    },
  },
    el("span", { class: "caret", text: "▶" }),
    el("h3", { text: repo.name }),
    el("span", { class: "repo-right" },
      summary.escalated ? el("span", { class: "tag tag-warn", text: `${summary.escalated} needs you` })
        : summary.running ? el("span", { class: "tag tag-busy", text: `${summary.running} running` })
        : el("span", { class: "muted", text: "idle" }),
      summary.last_activity ? el("span", { class: "muted", text: relativeTime(summary.last_activity) }) : null));

  if (tasks.length) {
    const rows = tasks.map((entry) => {
      const task = entry.task;
      return el("tr", {},
        el("td", {},
          el("a", { href: `#/task/${task.id}` }, el("strong", { text: task.title })),
          el("div", { class: "muted", style: "font-size:13px", text: projectSubtitle(task, entry) })),
        el("td", { class: "num" }, entry.forks
          ? `${entry.forks} · ${entry.running ? `${entry.running} running` : entry.merged === entry.forks ? "complete" : `${entry.queued} queued`}`
          : "—"),
        el("td", {}, tag(task.state)));
    });

    body.append(el("table", { class: "table" },
      el("thead", {}, el("tr", {},
        el("th", { text: "Project" }),
        el("th", { class: "num", text: "Sub-tasks" }),
        el("th", { text: "State" }))),
      el("tbody", {}, ...rows)));
  } else {
    body.append(empty("No projects in this repository yet."));
  }

  body.append(el("div", { class: "row-actions" },
    el("a", { class: "btn btn-ghost btn-sm", href: `#/new?repo=${repo.id}`, text: "+ New project in this repo" }),
    el("a", { class: "btn btn-ghost btn-sm", href: `#/repo/${repo.id}`, text: "Repository settings →" })));

  return el("div", { class: "repo" },
    head,
    el("div", { class: "repo-meta", text: [repo.default_branch, plural(tasks.length, "project"), repo.discovered ? null : "not yet discovered"].filter(Boolean).join(" · ") }),
    body);
}

// New project: one prompt, scoped to a repository.
async function renderNew(params) {
  const root = view();
  clear(root);

  const { repos } = await api.get("/v1/repos").catch(() => ({ repos: [] }));
  root.append(pageHead("New project", {
    crumbs: [{ label: "Repositories", href: "#/" }],
    lede: "One prompt, as many sub-tasks as it takes. Scope it to a repository first.",
  }));

  if (!repos?.length) {
    root.append(empty("Add a repository before starting a project."));
    return;
  }

  const preselect = params.get("repo");
  const repoSelect = el("select", { class: "input" },
    ...repos.map((repo) => el("option", { value: repo.id, text: repo.name, selected: repo.id === preselect })));

  const prompt = el("textarea", {
    class: "input",
    rows: "5",
    placeholder: "Describe the work. It will be split into sub-tasks you approve before anything runs.",
  });

  // Where finished sub-tasks land, and when, are per-project choices; neither
  // is assumed.
  const target = el("select", { class: "input" },
    el("option", { value: "default_branch", text: "the repository's default branch" }),
    el("option", { value: "integration_branch", text: "one shared branch for this project" }));
  const timing = el("select", { class: "input" },
    el("option", { value: "immediate", text: "as soon as each one is verified" }),
    el("option", { value: "batch", text: "together, once every sub-task is done" }));

  const submit = el("button", { class: "btn btn-primary", text: "Draft a plan" });
  submit.addEventListener("click", async () => {
    if (!prompt.value.trim()) { toast("Describe the work first.", true); return; }
    submit.disabled = true;
    try {
      const task = await api.send("POST", "/v1/tasks", {
        repo_id: repoSelect.value,
        request: prompt.value.trim(),
        merge_target: target.value,
        merge_timing: timing.value,
        plan: true,
      });
      location.hash = `#/task/${task.id}`;
    } catch (err) {
      toast(err.message, true);
      submit.disabled = false;
    }
  });

  root.append(
    el("div", { style: "max-width:620px" },
      el("div", { class: "field" }, el("label", { text: "Repository" }), repoSelect),
      el("div", { class: "field" }, el("label", { text: "What should happen?" }), prompt),
      el("div", { class: "field" }, el("label", { text: "Finished sub-tasks merge to" }), target),
      el("div", { class: "field" }, el("label", { text: "Merge them" }), timing),
      el("div", { class: "row-actions" }, submit,
        el("a", { class: "btn btn-secondary", href: "#/", text: "Cancel" }))),
    section("How a project runs",
      el("p", { class: "muted", style: "max-width:620px" },
        "Your prompt is split into sub-tasks you approve. Each one gets its own machine, " +
        "its own branch and its own preview URL, and they run in parallel — except where " +
        "two would collide, which are run one after another. A validation agent drives a " +
        "real browser against each preview and reports back.")));
}

// Project: one task and the sub-tasks under it.
async function renderTask(taskID) {
  const data = await api.get(`/v1/tasks/${taskID}`);
  const task = data.task;
  const forks = data.forks || [];
  const root = view();
  clear(root);

  const running = forks.filter((f) => !["merged", "abandoned", "failed", "queued", "escalated"].includes(f.state)).length;
  const lede = forks.length
    ? `${running} of ${plural(forks.length, "sub-task")} running`
    : task.state === "awaiting_plan" ? "Awaiting your approval" : stateLabel(task.state);

  root.append(pageHead(task.title, {
    crumbs: [{ label: "Repositories", href: "#/" }, { label: "Project" }],
    lede,
    aside: tag(task.state),
  }));

  const promptText = String(task.request || "").replace(/\s+/g, " ").trim();
  if (promptText && !promptText.startsWith(String(task.title || "").replace(/…$/, "").trim())) {
    root.append(el("p", { class: "muted", style: "max-width:70ch", text: promptText }));
  }

  const plan = task.plan;
  if (plan) {
    const open = (plan.questions || []).filter((q) => !q.answer);
    if (open.length) {
      root.append(section("Questions before anything runs", questionPanel(task, open)));
    }

    if (task.state === "awaiting_plan") {
      root.append(section(`Proposed plan · round ${plan.round}`,
        plan.summary ? el("p", { class: "muted", style: "max-width:70ch", text: plan.summary }) : null,
        ...(plan.workstreams || []).map((workstream, index) =>
          el("div", { class: "workstream" },
            el("h5", {}, `${index + 1}. `, workstream.name),
            el("p", { text: workstream.description }),
            workstream.serialize_group
              ? el("div", { class: "group" },
                  `runs after the other "${workstream.serialize_group}" sub-tasks`,
                  workstream.overlap_rationale ? ` — ${workstream.overlap_rationale}` : "")
              : null)),
        open.length
          ? el("p", { class: "muted" }, "Answer the questions above before approving.")
          : approvePanel(task, plan)));
    }
  }

  if (forks.length) {
    root.append(section("Sub-tasks", ...forks.map(subtaskRow)));
  }

  if (!["completed", "cancelled", "failed"].includes(task.state)) {
    root.append(el("div", { class: "row-actions" },
      el("button", {
        class: "btn btn-secondary btn-sm",
        text: "Cancel project",
        onclick: async () => {
          if (!confirm("Cancel this project? Unfinished sub-tasks are abandoned; their machines are left running.")) return;
          try {
            await api.send("POST", `/v1/tasks/${task.id}/cancel`, { reason: "cancelled from the control plane" });
            toast("Project cancelled.");
            renderTask(taskID);
          } catch (err) { toast(err.message, true); }
        },
      })));
  }

  root.append(section("Activity", await feed(`/v1/audit?task=${taskID}&limit=60`)));

  return live.subscribe(throttle((event) => {
    if (event.task_id === taskID && currentPath().startsWith("#/task/")) renderTask(taskID);
  }, 3000));
}

function approvePanel(task, plan) {
  const button = el("button", { class: "btn btn-primary", text: "Approve & boot machines" });
  button.addEventListener("click", async () => {
    button.disabled = true;
    try {
      await api.send("POST", `/v1/tasks/${task.id}/approve`);
      toast("Approved. Machines are being provisioned.");
      renderTask(task.id);
    } catch (err) {
      toast(err.message, true);
      button.disabled = false;
    }
  });

  const n = plan.workstreams?.length || 0;
  return el("div", { class: "row-actions" }, button,
    el("span", { class: "muted", text: `${plural(n, "machine")}, ${plural(n, "sub-task")}, ${plural(n, "preview URL")}` }));
}

function subtaskRow(fork) {
  return el("div", { class: "subtask" },
    el("div", { class: "subtask-head" },
      el("h5", {}, el("a", { href: `#/fork/${fork.id}`, style: "text-decoration:none", text: fork.name })),
      tag(fork.state),
      fork.serialize_group ? el("span", { class: "muted", style: "font-size:13px", text: `serialized · ${fork.serialize_group}` }) : null),
    fork.description ? el("p", { class: "muted", style: "margin:4px 0 0;font-size:14px", text: fork.description }) : null,
    el("div", { class: "subtask-meta" },
      el("span", { class: "mono", text: fork.branch }),
      fork.preview_url
        ? el("a", { href: fork.preview_url, target: "_blank", rel: "noopener noreferrer", text: "preview ↗" })
        : null,
      el("a", { href: `#/fork/${fork.id}`, text: "machine →" }),
      ...budgetMeters(fork.usage)));
}

function questionPanel(task, questions) {
  const panel = el("div", {});
  const answers = {};

  for (const question of questions) {
    const input = el("textarea", {
      class: "input", rows: "2", placeholder: "Your answer",
      oninput: (e) => { answers[question.id] = e.target.value; },
    });
    panel.append(el("div", { class: "question" },
      el("p", { text: question.text }),
      question.options?.length
        ? el("div", { class: "options" }, ...question.options.map((option) =>
            el("button", {
              class: "btn btn-secondary btn-sm", text: option,
              onclick: () => { input.value = option; answers[question.id] = option; },
            })))
        : null,
      input));
  }

  const send = el("button", { class: "btn btn-primary", text: "Send answers and replan" });
  send.addEventListener("click", async () => {
    const filled = Object.fromEntries(Object.entries(answers).filter(([, v]) => v && v.trim()));
    if (!Object.keys(filled).length) { toast("Answer at least one question first.", true); return; }
    send.disabled = true;
    try {
      await api.send("POST", `/v1/tasks/${task.id}/answers`, { answers: filled });
      toast("Answers sent; replanning.");
      renderTask(task.id);
    } catch (err) { toast(err.message, true); send.disabled = false; }
  });

  panel.append(el("div", { class: "row-actions" }, send));
  return panel;
}

// Sub-task: its machine, its preview, its transcript.
async function renderFork(forkID) {
  const data = await api.get(`/v1/forks/${forkID}`);
  const fork = data.fork;
  const usage = fork.usage || {};
  const remaining = data.budget_remaining || {};

  const root = view();
  clear(root);

  root.append(pageHead(fork.name, {
    crumbs: [
      { label: "Repositories", href: "#/" },
      { label: "Project", href: `#/task/${fork.task_id}` },
      { label: "Sub-task" },
    ],
    lede: fork.state_reason || stateLabel(fork.state),
    aside: tag(fork.state),
    row: fork.preview_url ? [
      el("span", { class: "mono", text: fork.preview_url.replace(/^https?:\/\//, "") }),
      el("a", { class: "btn btn-secondary btn-sm", href: fork.preview_url, target: "_blank", rel: "noopener noreferrer", text: "Open ↗" }),
      validateButton(fork),
    ] : [],
  }));

  if (fork.escalation && fork.state === "escalated") root.append(escalationPanel(fork));

  root.append(section("Machine",
    el("dl", { class: "kv" },
      el("dt", { text: "Status" }), el("dd", {}, stateLabel(fork.state)),
      el("dt", { text: "Machine" }), el("dd", { class: "mono", text: fork.instance_id || "not provisioned" }),
      el("dt", { text: "Branch" }), el("dd", { class: "mono", text: fork.branch }),
      fork.serialize_group ? el("dt", { text: "Serialized with" }) : null,
      fork.serialize_group ? el("dd", { text: fork.serialize_group }) : null,
      el("dt", { text: "Started" }), el("dd", { text: fork.started_at ? relativeTime(fork.started_at) : "not yet" }),
      el("dt", { text: "Validation rounds" }),
      el("dd", {}, `${usage.cycles || 0}`, remaining.cycles !== undefined ? el("span", { class: "muted", text: ` · ${remaining.cycles} left` }) : null),
      el("dt", { text: "Tokens" }), el("dd", { text: count((usage.input_tokens || 0) + (usage.output_tokens || 0)) }),
      el("dt", { text: "Spend" }),
      el("dd", {}, money(usage.cost_usd), remaining.cost_usd !== undefined ? el("span", { class: "muted", text: ` · ${money(remaining.cost_usd)} left` }) : null),
      el("dt", { text: "Working time" }), el("dd", { text: duration(usage.wall_ns) })),
    el("div", { class: "row-actions" }, ...budgetMeters(usage, remaining))));

  if (data.services && data.services.length) root.append(servicesSection(data.services, data.previews));

  root.append(workspacePanel({
    instance_id: fork.instance_id,
    shellPath: `/v1/forks/${fork.id}/shell`,
  }, data.previews || []));

  root.append(section("Transcript", await feed(`/v1/audit?fork=${forkID}&limit=100`)));

  const unsubscribe = live.subscribe(throttle((event) => {
    // A re-render tears down the workspace panel, and with it any open shell,
    // so only refresh on events that change what is displayed around it.
    if (event.fork_id === forkID && currentPath().startsWith("#/fork/") && !openShellCount) {
      renderFork(forkID);
    }
  }, 3000));
  return () => { unsubscribe(); closeOpenShells(); };
}

// A shell is a live session on a real machine; a background re-render that
// silently dropped it would look like the connection failing.
let openShellCount = 0;
const openShells = new Set();

function closeOpenShells() {
  for (const shell of openShells) shell.close();
  openShells.clear();
  openShellCount = 0;
}

// validateButton drives a browser against this sub-task's preview on request.
// It reports what was found without moving the sub-task: the pipeline owns the
// verify/fix loop, and this is a second opinion, not a restart of it.
function validateButton(fork) {
  const button = el("button", { class: "btn btn-secondary btn-sm", text: "Run validation" });
  button.addEventListener("click", async () => {
    button.disabled = true;
    button.textContent = "Validating…";
    try {
      const report = await api.send("POST", `/v1/forks/${fork.id}/validate`);
      toast(report.passed ? "Validation passed." : `Validation failed: ${report.summary}`, !report.passed);
      renderFork(fork.id);
    } catch (err) {
      toast(err.message, true);
      button.disabled = false;
      button.textContent = "Run validation";
    }
  });
  return button;
}

// servicesSection lists everything the project runs, not only what is
// reachable. An agent or a person debugging a broken preview needs to know a
// worker or a database is part of this project; its absence from the list
// would read as "there isn't one" rather than "it has no URL".

// serviceStatusTag says what a service is doing. Running and reachable are
// separate because a service can be up and not yet answering, which is what
// starting looks like and is not a failure.
function serviceStatusTag(status) {
  if (status.reachable) return el("span", { class: "svc-ok", text: " · answering" });
  if (status.running) return el("span", { class: "svc-wait", text: " · starting" });
  return el("span", { class: "svc-down", text: ` · not running${status.detail ? ` (${status.detail})` : ""}` });
}

// showServiceLogs opens a service's recent output.
async function showServiceLogs(workspaceID, name) {
  const root = view();
  clear(root);
  root.append(pageHead(`${name} log`, {
    crumbs: [
      { label: "Workspaces", href: "#/workspaces" },
      { label: "Workspace", href: `#/workspace/${workspaceID}` },
      { label: "Log" },
    ],
  }));
  const body = el("pre", { class: "service-log", text: "Reading…" });
  root.append(section("Recent output", body));
  try {
    const data = await api.get(`/v1/workspaces/${workspaceID}/services/${name}/logs`);
    // textContent, never innerHTML: this is a process's output.
    body.textContent = data.logs || "(nothing logged yet)";
  } catch (err) {
    body.textContent = err.message;
  }
}

function servicesSection(services, previews, options) {
  const urlFor = {};
  for (const entry of previews || []) {
    if (entry.service_id) urlFor[entry.service_id] = entry.url;
  }
  // Status is looked up live rather than stored: a recorded one goes stale
  // the moment a dev server falls over, and "running" when it is not is worse
  // than no answer.
  const statusFor = {};
  for (const entry of (options && options.status) || []) statusFor[entry.name] = entry;
  const workspaceID = options && options.workspaceID;

  const rows = services.map((service) => {
    const url = urlFor[service.id];
    const detail = [];
    if (service.port) detail.push(`port ${service.port}`);
    if (service.health_path) detail.push(`health ${service.health_path}`);
    if (service.depends_on && service.depends_on.length) {
      detail.push(`needs ${service.depends_on.join(", ")}`);
    }

    const status = statusFor[service.name];
    return el("div", { class: "service-row" },
      el("div", {},
        el("span", { class: "service-name", text: service.name }),
        el("span", { class: "muted", text: ` ${service.kind}` }),
        service.primary ? el("span", { class: "muted", text: " · front door" }) : null,
        status ? serviceStatusTag(status) : null,
        // The log is where the reason a service would not start actually is,
        // so it is one click from the service rather than somewhere else.
        workspaceID && service.command
          ? el("button", {
              class: "btn btn-secondary btn-sm service-logs",
              text: "Log",
              onclick: () => showServiceLogs(workspaceID, service.name),
            })
          : null),
      el("div", { class: "muted", text: detail.join(" · ") }),
      service.command
        ? el("div", { class: "mono muted service-command", text: service.command })
        : null,
      el("div", {},
        url
          ? el("a", {
              class: "mono", href: url, target: "_blank", rel: "noopener noreferrer",
              text: url.replace(/^https?:\/\//, ""),
            })
          // Say why rather than leaving a blank: "internal only" is a design
          // decision, and a datastore with a public URL would be a bug.
          : el("span", { class: "muted", text: service.exposed ? "not reachable yet" : "internal only" })));
  });

  return section("Services", el("div", { class: "services" }, ...rows));
}


// --- Workspaces -----------------------------------------------------------
//
// A workspace is somewhere you work: a branch, a machine, the services on it
// and a preview. Unlike a sub-task it has no notion of finishing, so the
// controls are start and stop rather than approve and merge.

const WORKSPACE_LABELS = {
  stopped: "Stopped",
  starting: "Starting",
  running: "Running",
  stopping: "Stopping",
  failed: "Failed to start",
  archived: "Archived",
};

function workspaceState(state) {
  return WORKSPACE_LABELS[state] || state;
}

async function renderWorkspaces() {
  const [data, repoData] = await Promise.all([
    api.get("/v1/workspaces"),
    api.get("/v1/repos"),
  ]);
  const workspaces = data.workspaces || [];
  const repos = repoData.repos || [];

  const root = view();
  clear(root);
  root.append(pageHead("Your workspaces", {
    lede: "A branch, a machine and a live preview you can keep open while you change things.",
    row: repos.length ? [newWorkspaceButton(repos)] : [],
  }));

  if (!repos.length) {
    root.append(empty("Add a repository first; a workspace runs one of its projects."));
    return;
  }
  if (!workspaces.length) {
    root.append(empty("No workspaces yet. Open one to start working on a branch."));
    return;
  }

  const list = el("div", { class: "services" });
  for (const ws of workspaces) {
    list.append(el("div", { class: "service-row" },
      el("div", {},
        el("a", { class: "service-name", href: `#/workspace/${ws.id}`, text: ws.name })),
      el("div", { class: "muted", text: `${workspaceState(ws.state)} · ${ws.branch}` }),
      null,
      el("div", {},
        ws.preview_url
          ? el("a", {
              class: "mono", href: ws.preview_url, target: "_blank", rel: "noopener noreferrer",
              text: ws.preview_url.replace(/^https?:\/\//, ""),
            })
          : el("span", { class: "muted", text: ws.state === "running" ? "no front door" : "not running" }))));
  }
  root.append(section("Workspaces", list));
}

// newWorkspaceButton opens the smallest form that can create one.
function newWorkspaceButton(repos) {
  const button = el("button", { class: "btn", text: "New workspace" });
  button.addEventListener("click", () => {
    const root = view();
    clear(root);
    root.append(pageHead("New workspace", {
      crumbs: [{ label: "Workspaces", href: "#/workspaces" }, { label: "New" }],
      lede: "It starts stopped unless you ask for it to come up now.",
    }));

    const repoSelect = el("select", { class: "input" });
    for (const repo of repos) {
      repoSelect.append(el("option", { value: repo.id, text: repo.name }));
    }
    const name = el("input", { class: "input", placeholder: "what you are working on" });
    const branch = el("input", { class: "input", placeholder: "branch (blank: the repo default)" });
    const startNow = el("input", { type: "checkbox" });
    const problem = el("p", { class: "muted" });

    const submit = el("button", { class: "btn", text: "Create" });
    submit.addEventListener("click", async () => {
      submit.disabled = true;
      clear(problem);
      try {
        const created = await api.send("POST", "/v1/workspaces", {
          repo_id: repoSelect.value,
          name: name.value,
          branch: branch.value,
          start: startNow.checked,
        });
        location.hash = `#/workspace/${created.workspace.id}`;
      } catch (err) {
        problem.textContent = err.message;
        submit.disabled = false;
      }
    });

    root.append(section("Details",
      el("dl", { class: "kv" },
        el("dt", { text: "Repository" }), el("dd", {}, repoSelect),
        el("dt", { text: "Name" }), el("dd", {}, name),
        el("dt", { text: "Branch" }), el("dd", {}, branch),
        el("dt", { text: "Start now" }), el("dd", {}, startNow)),
      el("div", { class: "row-actions" }, submit,
        el("a", { class: "btn btn-secondary", href: "#/workspaces", text: "Cancel" })),
      problem));
  });
  return button;
}

async function renderWorkspace(workspaceID) {
  const data = await api.get(`/v1/workspaces/${workspaceID}`);
  const ws = data.workspace;
  const previews = data.previews || [];

  const root = view();
  clear(root);
  root.append(pageHead(ws.name, {
    crumbs: [{ label: "Workspaces", href: "#/workspaces" }, { label: "Workspace" }],
    lede: ws.state_reason || workspaceState(ws.state),
    aside: tag(ws.state),
    row: workspaceControls(ws),
  }));

  root.append(section("Environment",
    el("dl", { class: "kv" },
      el("dt", { text: "Status" }), el("dd", {}, workspaceState(ws.state)),
      el("dt", { text: "Branch" }), el("dd", { class: "mono", text: ws.branch }),
      el("dt", { text: "Machine" }),
      el("dd", { class: "mono", text: ws.instance_id || "none yet" }),
      el("dt", { text: "Last used" }),
      el("dd", { text: ws.last_used_at ? relativeTime(ws.last_used_at) : "never" }))));

  if (data.services && data.services.length) {
    let status = null;
    try {
      status = (await api.get(`/v1/workspaces/${ws.id}/services`)).status;
    } catch {
      // The definitions are still worth showing; a machine that cannot be
      // reached is itself part of the answer.
    }
    root.append(servicesSection(data.services, previews, { status, workspaceID: ws.id }));
  }

  // The same workspace panel sub-tasks use: the machine and its previews are
  // the same kind of thing either way.
  root.append(workspacePanel({
    id: ws.id,
    instance_id: ws.instance_id,
    shellPath: `/v1/workspaces/${ws.id}/shell`,
    // Only a workspace holds a conversation: a sub-task is driven by its
    // plan, not by somebody typing at it.
    conversation: true,
    running: ws.state === "running",
  }, previews));

  root.append(section("Activity", await feed(`/v1/audit?workspace=${ws.id}&limit=100`)));

  // Follow its lifecycle live: starting takes minutes, and a page that does
  // not move looks like one that is broken. Only lifecycle events, though --
  // rebuilding the view on every agent message would throw away whatever is
  // half-typed in the chat, which keeps itself current on its own.
  return live.subscribe((event) => {
    if (event.workspace_id === ws.id && String(event.type).startsWith("workspace.")) route();
  });
}

// workspaceControls are the lifecycle buttons for the current state.
function workspaceControls(ws) {
  const act = async (verb) => {
    try {
      await api.send("POST", `/v1/workspaces/${ws.id}/${verb}`, {});
      route();
    } catch (err) {
      alert(err.message);
    }
  };

  const controls = [];
  if (ws.state === "stopped" || ws.state === "failed") {
    controls.push(el("button", { class: "btn", text: "Start", onclick: () => act("start") }));
  }
  if (ws.state === "running" || ws.state === "starting") {
    controls.push(el("button", { class: "btn btn-secondary", text: "Stop", onclick: () => act("stop") }));
  }
  if (ws.state === "stopped" || ws.state === "failed") {
    controls.push(el("button", {
      class: "btn btn-secondary", text: "Archive",
      onclick: () => {
        // Terminal, so it is worth one question.
        if (confirm(`Archive ${ws.name}? You cannot start it again.`)) act("archive");
      },
    }));
  }
  if (ws.preview_url) {
    controls.push(el("a", {
      class: "btn btn-secondary", href: ws.preview_url,
      target: "_blank", rel: "noopener noreferrer", text: "Open ↗",
    }));
  }
  return controls;
}


// chatPane is the conversation with the agent about this workspace.
//
// Deliberately plain. The agent takes minutes, so the pane is mostly about
// making waiting legible: the turn in flight is shown as running, and the
// event stream is what moves it on rather than a poll.
function chatPane(owner) {
  const host = el("div", { class: "chat" });
  const log = el("div", { class: "chat-log" });
  const input = el("textarea", {
    class: "input chat-input", rows: "3",
    placeholder: owner.running
      ? "Ask for a change, or a question about the code"
      : "Start the workspace before asking the agent for anything",
  });
  const send = el("button", { class: "btn", text: "Send" });
  const problem = el("p", { class: "muted" });
  input.disabled = !owner.running;
  send.disabled = !owner.running;

  let busy = false;

  const draw = (messages) => {
    clear(log);
    if (!messages.length) {
      log.append(empty("Nothing asked yet. Describe a change and the agent will make it on this machine."));
      return;
    }
    busy = false;
    for (const m of messages) {
      if (m.state === "running") busy = true;
      const who = m.role === "user" ? "You" : "Agent";
      const entry = el("div", { class: `chat-turn chat-${m.role}` },
        el("div", { class: "chat-who" },
          el("span", { text: who }),
          el("span", { class: "muted", text: ` · ${relativeTime(m.created_at)}` }),
          m.usage && m.usage.cost_usd
            ? el("span", { class: "muted", text: ` · ${money(m.usage.cost_usd)}` })
            : null));
      if (m.state === "running" && !m.content) {
        entry.append(el("p", { class: "muted", text: "Working…" }));
      } else {
        // Never innerHTML: the agent's output is untrusted and lands as text.
        entry.append(el("p", { class: "chat-body", text: m.content || "(nothing said)" }));
        // A running turn's content is the answer so far, so say that rather
        // than letting a half-written sentence read as a finished one.
        if (m.state === "running") {
          entry.append(el("p", { class: "muted chat-writing", text: "still writing…" }));
        }
      }
      if (m.state === "failed" && m.state_reason) {
        entry.append(el("p", { class: "muted", text: m.state_reason }));
      }
      log.append(entry);
    }
    log.scrollTop = log.scrollHeight;
    // input.value is untouched throughout: only the log is redrawn, so a
    // half-written message survives the agent answering the previous one.
    // A run in flight owns the machine, so a second request would be two
    // agents changing the same files.
    input.disabled = !owner.running || busy;
    send.disabled = !owner.running || busy;
  };

  const refresh = async () => {
    try {
      draw((await api.get(`/v1/workspaces/${owner.id}/messages`)).messages || []);
    } catch (err) {
      problem.textContent = err.message;
    }
  };

  const submit = async () => {
    const content = input.value.trim();
    if (!content) return;
    send.disabled = true;
    clear(problem);
    try {
      await api.send("POST", `/v1/workspaces/${owner.id}/messages`, { content });
      input.value = "";
      await refresh();
    } catch (err) {
      problem.textContent = err.message;
      send.disabled = false;
    }
  };

  // Refresh in place when the agent says something, rather than letting the
  // whole view rebuild: a redraw while somebody is typing throws their draft
  // away, and the agent can take minutes to answer.
  const unsubscribe = live.subscribe((event) => {
    if (event.workspace_id === owner.id) refresh();
  });

  // While a turn is in flight, poll as well. The answer arrives as text long
  // before the event that says the turn finished, and an interface that only
  // moves at the end is the thing streaming exists to replace.
  const poll = setInterval(() => {
    if (busy) refresh();
  }, 1200);

  host.dispose = () => {
    unsubscribe();
    clearInterval(poll);
  };

  send.addEventListener("click", submit);
  input.addEventListener("keydown", (event) => {
    // Enter sends, shift+enter is a newline: this is a chat, not an editor.
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      submit();
    }
  });

  host.append(log, el("div", { class: "chat-compose" }, input, send), problem);
  refresh();
  return host;
}

// workspacePanel is the sub-task's workspace: the live previews it produces and
// a shell on the machine producing it.
//
// A project runs a set of services, so there may be several addresses worth
// looking at. The pane shows one at a time with a switcher, because an iframe
// per service would load every dev server at once.
function workspacePanel(owner, previews) {
  const body = el("div", {});
  const tabs = el("div", { class: "tabs-inline", role: "tablist" });
  let shell = null;
  let chat = null;

  // Front door first: it is what the validation agent drives.
  const reachable = previews.slice().sort((a, b) => (b.primary === true) - (a.primary === true));
  let shownService = reachable.length ? reachable[0] : null;

  const previewPane = () => {
    if (!shownService) {
      return empty(previews.length
        ? "No service in this project is reachable yet."
        : "No preview yet. It appears once the machine has booted.");
    }
    const frame = el("iframe", {
      class: "preview-frame", src: shownService.url, loading: "lazy",
      sandbox: "allow-scripts allow-same-origin allow-forms",
    });
    const note = shownService.primary
      ? "Reloads on every agent commit. This is the same URL the validation agent drives."
      : `Reloads on every agent commit. This is the ${shownService.service} service, not the page the validation agent drives.`;

    const header = el("div", { class: "row-actions", style: "margin-bottom:10px" });
    if (reachable.length > 1) {
      // One button per reachable service. Switching replaces the iframe, so
      // only the service being looked at is ever loaded.
      for (const entry of reachable) {
        const label = entry.primary ? `${entry.service} (front door)` : entry.service;
        const button = el("button", {
          class: entry.service_id === shownService.service_id ? "btn btn-sm" : "btn btn-secondary btn-sm",
          text: label,
        });
        button.addEventListener("click", () => {
          if (entry.service_id === shownService.service_id) return;
          shownService = entry;
          show("preview", true);
        });
        header.append(button);
      }
    }
    header.append(el("a", {
      class: "btn btn-secondary btn-sm", href: shownService.url,
      target: "_blank", rel: "noopener noreferrer", text: "Open ↗",
    }));

    return el("div", {},
      header,
      el("p", { class: "muted", style: "margin-bottom:10px" }, note),
      el("p", { class: "mono muted", style: "margin-bottom:10px" },
        shownService.url.replace(/^https?:\/\//, "")),
      frame);
  };

  const panes = {
    chat: () => chatPane(owner),
    preview: previewPane,
    shell: () => {
      if (!owner.instance_id) return empty("No machine yet, so there is nothing to attach to.");
      const host = el("div", { class: "shell" });
      // Mount after the element is in the document: the terminal measures
      // itself, and measuring a detached node gives a useless size.
      queueMicrotask(() => { shell = openShell(owner, host); });
      return el("div", {},
        el("p", { class: "muted", style: "margin-bottom:10px" },
          "The same machine the agents are on. Anything you change here, they see."),
        host);
    },
  };

  let active = owner.conversation ? "chat" : "preview";
  const content = el("div", {});

  const show = (name, force) => {
    if (name === active && content.firstChild && !force) return;
    active = name;
    // Leaving the shell tab closes the session rather than leaving a terminal
    // running on the machine behind a tab nobody is looking at.
    if (name !== "shell" && shell) { shell.close(); shell = null; }
    // The chat's event subscription goes with it, or every tab switch would
    // leave another one refreshing a log nobody can see.
    if (name !== "chat" && chat) { chat(); chat = null; }
    clear(content);
    const pane = panes[name]();
    if (name === "chat" && pane.dispose) chat = pane.dispose;
    content.append(pane);
    for (const button of tabs.children) {
      button.setAttribute("aria-selected", String(button.dataset.pane === name));
    }
  };

  const tabNames = owner.conversation
    ? [["chat", "Chat"], ["preview", "Preview"], ["shell", "Shell"]]
    : [["preview", "Preview"], ["shell", "Shell"]];
  for (const [name, label] of tabNames) {
    const button = el("button", { role: "tab", "aria-selected": String(name === active), text: label });
    button.dataset.pane = name;
    button.addEventListener("click", () => show(name));
    tabs.append(button);
  }

  body.append(tabs, content);
  show(active);
  return section("Workspace", body);
}

// openShell attaches a terminal to the sub-task's machine over a websocket.
function openShell(target, host) {
  const term = new window.Terminal({
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
    fontSize: 13,
    cursorBlink: true,
    theme: { background: "#161514", foreground: "#e6e3de", cursor: "#d8808c" },
  });
  const fit = new window.FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(host);
  fit.fit();

  const scheme = location.protocol === "https:" ? "wss" : "ws";
  // The path is given rather than derived: a sub-task's machine and a
  // workspace's are the same terminal on different owners.
  const socket = new WebSocket(
    `${scheme}://${location.host}${target.shellPath}?cols=${term.cols}&rows=${term.rows}`);
  socket.binaryType = "arraybuffer";

  const send = (message) => {
    if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(message));
  };

  socket.onmessage = (event) => {
    // Output arrives as raw bytes: a terminal's stream is not necessarily
    // valid UTF-8, so it cannot travel as text.
    term.write(typeof event.data === "string" ? event.data : new Uint8Array(event.data));
  };
  socket.onclose = () => term.write("\r\n\x1b[2mdabberz: session ended\x1b[0m\r\n");
  socket.onerror = () => term.write("\r\n\x1b[31mdabberz: could not reach the machine\x1b[0m\r\n");

  term.onData((data) => send({ t: "input", d: data }));

  const resize = () => {
    fit.fit();
    send({ t: "resize", c: term.cols, r: term.rows });
  };
  window.addEventListener("resize", resize);

  const handle = {
    close() {
      if (!openShells.has(handle)) return;
      openShells.delete(handle);
      openShellCount = openShells.size;
      window.removeEventListener("resize", resize);
      socket.close();
      term.dispose();
    },
  };
  openShells.add(handle);
  openShellCount = openShells.size;
  return handle;
}

function escalationPanel(fork) {
  const escalation = fork.escalation;
  const response = el("textarea", { class: "input", rows: "3", placeholder: "Tell the agent what to do" });
  const resume = el("select", { class: "input", style: "width:auto" },
    ...["coding", "fixing", "verifying", "merging"].map((state) =>
      el("option", { value: state, text: `resume at ${stateLabel(state)}` })));

  const send = el("button", { class: "btn btn-primary", text: "Answer and resume" });
  send.addEventListener("click", async () => {
    if (!response.value.trim()) { toast("Write an answer first.", true); return; }
    send.disabled = true;
    try {
      await api.send("POST", `/v1/forks/${fork.id}/resolve`, {
        response: response.value.trim(), resume: resume.value,
      });
      toast("Sub-task resumed.");
      renderFork(fork.id);
    } catch (err) { toast(err.message, true); send.disabled = false; }
  });

  return el("div", { class: "callout" },
    el("h4", { text: `Waiting for you — ${stateLabel(escalation.kind)}` }),
    el("p", { text: escalation.message }),
    escalation.detail ? el("pre", { text: escalation.detail }) : null,
    response,
    el("div", { class: "row-actions" }, resume, send));
}

// Repository: what dabberz knows about one repo.
async function renderRepo(repoID) {
  const [repo, projects, secrets, overview] = await Promise.all([
    api.get(`/v1/repos/${repoID}`),
    api.get(`/v1/repos/${repoID}/projects`).catch(() => ({ projects: [] })),
    api.get(`/v1/repos/${repoID}/secrets`).catch(() => ({ secrets: [] })),
    api.get("/v1/overview").catch(() => ({ repos: [] })),
  ]);

  const summary = (overview.repos || []).find((entry) => entry.repo.id === repoID);
  const root = view();
  clear(root);

  root.append(pageHead(repo.name, {
    crumbs: [{ label: "Repositories", href: "#/" }],
    lede: repo.remote_url,
    aside: el("a", { class: "btn btn-secondary", href: `#/new?repo=${repoID}`, text: "New project" }),
    row: [
      el("span", { class: "muted", text: `default branch ${repo.default_branch}` }),
      repo.discovered ? null : el("span", { class: "tag tag-idle", text: "layout not yet confirmed" }),
    ],
  }));

  if (summary?.tasks?.length) {
    root.append(section("Projects", ...summary.tasks.map((entry) =>
      el("div", { class: "subtask" },
        el("div", { class: "subtask-head" },
          el("h5", {}, el("a", { href: `#/task/${entry.task.id}`, style: "text-decoration:none", text: entry.task.title })),
          tag(entry.task.state)),
        el("div", { class: "subtask-meta" },
          el("span", { text: entry.forks ? plural(entry.forks, "sub-task") : "no sub-tasks yet" }),
          entry.preview_url
            ? el("a", { href: entry.preview_url, target: "_blank", rel: "noopener noreferrer", text: "preview ↗" })
            : null)))));
  }

  root.append(section("Detected projects",
    projects.projects?.length
      ? el("table", { class: "table" },
          el("thead", {}, el("tr", {},
            el("th", { text: "Name" }), el("th", { text: "Path" }),
            el("th", { text: "Toolchain" }), el("th", { text: "Confirmed" }))),
          el("tbody", {}, ...projects.projects.map((project) =>
            el("tr", {},
              el("td", { text: project.name }),
              el("td", { class: "mono", text: project.path }),
              el("td", { text: project.toolchain || "—" }),
              el("td", {}, project.confirmed
                ? el("span", { class: "tag tag-good", text: "confirmed" })
                : el("span", { class: "tag tag-idle", text: "inferred" }))))))
      : empty("The repository layout has not been inspected yet.")));

  root.append(section("Secrets",
    secrets.secrets?.length
      ? el("div", { class: "row-actions" }, ...secrets.secrets.map((name) =>
          el("span", { class: "tag tag-idle mono", text: name })))
      : empty("No secrets set for this repository."),
    el("p", { class: "muted", style: "font-size:13px;margin-top:10px" },
      "Inherited by every sub-task under this repository. Values are never displayed.")));

  root.append(section("Activity", await feed(`/v1/audit?repo=${repoID}&limit=40`)));
}

// Activity: the unified trail, across every repository.
async function renderActivity(params) {
  const root = view();
  clear(root);

  const state = {
    repo: params.get("repo") || "",
    actor: params.get("actor") || "",
    follow: params.get("follow") !== "0",
  };

  root.append(pageHead("Activity", { lede: "Every action, across every repository." }));

  const { repos } = await api.get("/v1/repos").catch(() => ({ repos: [] }));
  const rows = el("div", {});

  const repoSelect = el("select", { class: "input", style: "width:auto", onchange: (e) => { state.repo = e.target.value; reload(); } },
    el("option", { value: "", text: "All repositories" }),
    ...(repos || []).map((repo) => el("option", { value: repo.id, text: repo.name, selected: repo.id === state.repo })));

  const actorSelect = el("select", { class: "input", style: "width:auto", onchange: (e) => { state.actor = e.target.value; reload(); } },
    el("option", { value: "", text: "Everyone" }));

  const followBox = el("input", { type: "checkbox", checked: state.follow, onchange: (e) => { state.follow = e.target.checked; } });

  root.append(el("div", { class: "filters" },
    repoSelect, actorSelect,
    el("label", { class: "toggle" }, followBox, "Follow live"),
    el("button", { class: "btn btn-secondary btn-sm", onclick: () => reload(), text: "Refresh" })));
  root.append(rows);

  const more = el("div", { class: "row-actions" });
  root.append(more);
  let oldest = 0;

  async function load(before) {
    const query = new URLSearchParams({ limit: "100" });
    if (state.repo) query.set("repo", state.repo);
    if (state.actor) query.set("actor", state.actor);
    if (before) query.set("before", String(before));
    return api.get(`/v1/audit?${query}`);
  }

  async function reload() {
    const data = await load(0);
    clear(rows);
    clear(more);

    if (!data.events?.length) {
      rows.append(empty("No activity matches this filter."));
    } else {
      for (const event of data.events) rows.append(eventRow(event));
      oldest = data.events[data.events.length - 1].seq;
      if (data.events.length >= 100) more.append(el("button", { class: "btn btn-secondary btn-sm", onclick: older, text: "Load older" }));
    }

    if (actorSelect.options.length === 1 && data.actors) {
      for (const actor of data.actors) {
        actorSelect.append(el("option", { value: actor, text: actor, selected: actor === state.actor }));
      }
    }
  }

  async function older() {
    const data = await load(oldest);
    clear(more);
    if (!data.events?.length) {
      more.append(el("span", { class: "muted", text: "Beginning of the trail." }));
      return;
    }
    for (const event of data.events) rows.append(eventRow(event));
    oldest = data.events[data.events.length - 1].seq;
    more.append(el("button", { class: "btn btn-secondary btn-sm", onclick: older, text: "Load older" }));
  }

  await reload();

  return live.subscribe((event) => {
    if (!state.follow) return;
    if (state.repo && event.repo_id !== state.repo) return;
    if (state.actor && event.actor !== state.actor) return;
    const placeholder = rows.querySelector(".empty");
    if (placeholder) placeholder.remove();
    rows.prepend(eventRow(event, true));
  });
}

// ---------------------------------------------------------------- routing

const currentPath = () => (location.hash || "#/").split("?")[0];

let disposeView = null;


// ---------------------------------------------------------------- account

// The account page. Its job is one question: can this person's own GitHub
// credential be used to clone and push, and if not, what do they do about it.
//
// The connection itself is made in the identity provider's account UI, which
// is what makes it self-serve: dabberz never sees the OAuth exchange and never
// stores a token. This page reports and explains, and hands off for the rest.
async function renderAccount() {
  const root = view();
  clear(root);
  root.append(pageHead("Your account", {
    lede: "Who you are signed in as, and what dabberz can do on your behalf.",
  }));

  const [who, link] = await Promise.all([
    api.get("/v1/auth/whoami").catch(() => null),
    api.get("/v1/github").catch((err) => ({ github: { blocked: err.message } })),
  ]);

  if (who?.user) {
    const identity = el("div", { class: "section" },
      el("h6", { text: "Signed in" }),
      el("p", { text: who.user.email || who.user.id }),
      who.provider ? el("p", { class: "muted", text: `Identity provided by ${who.provider}.` }) : null,
    );
    root.append(identity);
  }

  const host = el("div", { class: "section" });
  root.append(host);
  renderGithubLink(host, link?.github || {});
}

// renderGithubLink draws the connection state and the one action that moves it
// forward. Redrawn in place rather than re-routing, so a recheck does not
// scroll the page back to the top.
function renderGithubLink(host, github) {
  clear(host);
  host.append(el("h6", { text: "GitHub" }));

  const connected = Boolean(github.connected);
  const usable = Boolean(github.usable);
  const tone = usable ? "good" : connected ? "warn" : "idle";
  const words = usable ? "connected" : connected ? "connected, not usable" : "not connected";

  host.append(el("p", {},
    el("span", { class: `tag tag-${tone}`, text: words }),
    github.account ? el("span", { class: "muted", text: ` ${github.account}` }) : null,
  ));

  if (usable) {
    host.append(el("p", { class: "muted", text:
      "Workspaces you start clone with your own GitHub credential. It is never stored by dabberz: " +
      "it is read from your identity provider when a clone needs one, and it is handed to git only — " +
      "not to the machine, the agent, or the services your project runs." }));
    if (github.scopes?.length) {
      host.append(el("p", { class: "muted", text: `Permissions granted: ${github.scopes.join(", ")}.` }));
    }
    if (!github.scopes?.includes("repo")) {
      host.append(el("p", { class: "muted", text:
        "Private repositories need the repo scope. Without it public repositories still clone." }));
    }
  } else if (github.blocked) {
    host.append(el("p", { text: github.blocked }));
  }

  const row = el("div", { class: "actions" });
  if (window.Clerk?.openUserProfile) {
    row.append(el("button", {
      class: connected ? "btn btn-secondary" : "btn btn-primary",
      text: connected ? "Manage connected accounts" : "Connect GitHub",
      onclick: () => {
        try {
          window.Clerk.openUserProfile();
        } catch (err) {
          toast(err.message, true);
        }
      },
    }));
  }
  if (github.configured) {
    row.append(el("button", {
      class: "btn btn-secondary",
      text: "Check again",
      onclick: async (event) => {
        const button = event.currentTarget;
        button.disabled = true;
        try {
          const fresh = await api.send("POST", "/v1/github/recheck");
          renderGithubLink(host, fresh.github || {});
        } catch (err) {
          toast(err.message, true);
          button.disabled = false;
        }
      },
    }));
  }
  host.append(row);
}

const ROUTES = [
  [/^#\/$/, () => renderRepos(), "repos"],
  [/^#\/workspaces$/, () => renderWorkspaces(), "workspaces"],
  [/^#\/workspace\/([\w-]+)$/, (m) => renderWorkspace(m[1]), "workspaces"],
  [/^#\/activity$/, (_, params) => renderActivity(params), "activity"],
  [/^#\/account$/, () => renderAccount(), "account"],
  [/^#\/new$/, (_, params) => renderNew(params), "repos"],
  [/^#\/repo\/([\w-]+)$/, (m) => renderRepo(m[1]), "repos"],
  [/^#\/task\/([\w-]+)$/, (m) => renderTask(m[1]), "repos"],
  [/^#\/fork\/([\w-]+)$/, (m) => renderFork(m[1]), "repos"],
];

async function route() {
  // Suspended while the sign-in form is up; see signingIn.
  if (signingIn) return;
  if (disposeView) { disposeView(); disposeView = null; }

  const [path, queryString = ""] = (location.hash || "#/").split("?");
  const params = new URLSearchParams(queryString);

  for (const [pattern, render, tab] of ROUTES) {
    const match = path.match(pattern);
    if (!match) continue;

    highlightTab(tab);
    try {
      disposeView = (await render(match, params)) || null;
    } catch (err) {
      const root = view();
      clear(root);
      root.append(pageHead("Something went wrong"));
      root.append(el("p", { text: err.message }));
      root.append(el("button", { class: "btn btn-secondary", text: "Retry", onclick: () => route() }));
    }
    window.scrollTo(0, 0);
    return;
  }

  const root = view();
  clear(root);
  root.append(pageHead("Not found"));
  root.append(el("a", { href: "#/", text: "Back to your repositories" }));
}

function highlightTab(active) {
  for (const anchor of document.querySelectorAll(".tabs a")) {
    anchor.classList.toggle("active", anchor.dataset.route === active);
  }
}

function throttle(fn, interval) {
  let last = 0;
  let timer = null;
  return (...args) => {
    const wait = interval - (Date.now() - last);
    clearTimeout(timer);
    if (wait <= 0) { last = Date.now(); fn(...args); }
    else timer = setTimeout(() => { last = Date.now(); fn(...args); }, wait);
  };
}

// projectSubtitle describes the shape of a project rather than repeating its
// title. A title auto-derived from the prompt is the prompt, so echoing it
// underneath tells the reader nothing.
function projectSubtitle(task, counts = {}) {
  const request = String(task.request || "").replace(/\s+/g, " ").trim();
  const title = String(task.title || "").replace(/…$/, "").trim();
  if (request && !request.startsWith(title)) return truncate(request, 90);
  if (counts.forks) return `One prompt, ${plural(counts.forks, "sub-task")}`;
  if (task.plan?.workstreams?.length) return `One prompt, ${plural(task.plan.workstreams.length, "sub-task")} proposed`;
  return "Not yet planned";
}

function truncate(text, limit) {
  const clean = String(text || "").replace(/\s+/g, " ").trim();
  return clean.length <= limit ? clean : clean.slice(0, limit) + "…";
}

window.addEventListener("hashchange", route);

// Sign-in screen. Deliberately plain: this is the one part of the interface
// expected to be replaced wholesale by a real web application, so it gets no
// more than it needs.
// signingIn suspends this application's router. The sign-in component may
// navigate for its own steps, and whatever it pushes is not a route here: left
// unguarded, entering an email replaced the form with "Not found" half way
// through signing in. Virtual routing is requested as well, but the guard is
// what makes it safe regardless of whether the provider honours it.
let signingIn = false;

function renderSignIn(reason) {
  signingIn = true;
  live.stop();
  // Otherwise the header reads "connecting" behind the sign-in form, which is
  // not true and suggests the control plane is unreachable.
  setConnection("signed-out");
  const root = view();
  clear(root);
  root.append(pageHead("Sign in", {
    lede: reason || "This control plane runs agents, holds repository secrets and offers a shell on its machine. It needs to know who you are.",
  }));
  const host = el("div", { id: "sign-in", style: "margin-top:20px" });
  root.append(host);
  if (!window.Clerk) {
    host.append(empty("The sign-in provider could not be loaded."));
    return;
  }
  try {
    // routing: "virtual" keeps the component's steps out of the URL.
    // Its default is hash routing, and this application already owns the
    // hash: a multi-step sign-in would push #/factor-one, our router would
    // not recognise it, and the form would be replaced by "Not found"
    // half way through signing in.
    window.Clerk.mountSignIn(host, { routing: "virtual" });
  } catch (err) {
    host.append(empty("The sign-in form could not be shown: " + err.message));
  }
}

// loadClerk pulls the provider's script from its own instance host rather than
// a third-party CDN: that host is already the issuer whose signing keys the
// server trusts, so it adds no new party to the system.
function loadClerk(config) {
  return new Promise((resolve, reject) => {
    const script = document.createElement("script");
    script.src = `https://${config.frontend_api}/npm/@clerk/clerk-js@5.38.0/dist/clerk.browser.js`;
    script.async = true;
    script.crossOrigin = "anonymous";
    script.dataset.clerkPublishableKey = config.publishable_key;
    script.addEventListener("load", resolve);
    script.addEventListener("error", () => reject(new Error("could not load the sign-in provider")));
    document.head.append(script);
  });
}

async function start() {
  let config;
  try {
    config = await (await fetch("/v1/auth/config", { headers: { accept: "application/json" } })).json();
  } catch {
    // Without knowing how to sign in there is nothing useful to show, and
    // starting the pollers would produce a wall of failures.
    const root = view();
    clear(root);
    root.append(empty("The control plane is not reachable."));
    return;
  }

  if (config.sign_in_required) {
    try {
      await loadClerk(config);
      await window.Clerk.load({ publishableKey: config.publishable_key });
    } catch (err) {
      const root = view();
      clear(root);
      root.append(pageHead("Sign in unavailable", { lede: err.message }));
      return;
    }
    if (!window.Clerk.user) {
      // With virtual routing the provider never navigates, so nothing would
      // otherwise tell us sign-in finished. Start again once a user exists.
      window.Clerk.addListener(({ user }) => {
        if (user) start();
      });
      renderSignIn();
      return;
    }
  }

  signingIn = false;
  live.start();
  refreshStatus();
  setInterval(refreshStatus, 5000);
  route();
}

start();
