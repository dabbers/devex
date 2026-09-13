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

const api = {
  async get(path) {
    const res = await fetch(path, { headers: { accept: "application/json" } });
    if (!res.ok) throw new Error(await describe(res));
    return res.json();
  },
  async send(method, path, body) {
    const res = await fetch(path, {
      method,
      headers: body ? { "content-type": "application/json" } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
    if (!res.ok) throw new Error(await describe(res));
    return res.status === 204 ? null : res.json();
  },
};

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

  start() {
    this.connect();
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && (!this.source || this.source.readyState === 2)) this.connect();
    });
  },

  connect() {
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
    state === "live" ? "live" : state === "down" ? "reconnecting" : "connecting";
}

// ---------------------------------------------------------------- status bar

async function refreshStatus() {
  try {
    const capacity = await api.get("/v1/capacity");
    const strip = document.getElementById("status-strip");
    clear(strip);

    const machines = capacity.active_forks || 0;
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

  root.append(workspacePanel(fork));

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

// workspacePanel is the sub-task's workspace: the live preview it produces and
// a shell on the machine producing it.
function workspacePanel(fork) {
  const body = el("div", {});
  const tabs = el("div", { class: "tabs-inline", role: "tablist" });
  let shell = null;

  const panes = {
    preview: () => fork.preview_url
      ? el("div", {},
          el("p", { class: "muted", style: "margin-bottom:10px" },
            "Reloads on every agent commit. This is the same URL the validation agent drives."),
          el("iframe", {
            class: "preview-frame", src: fork.preview_url, loading: "lazy",
            sandbox: "allow-scripts allow-same-origin allow-forms",
          }))
      : empty("No preview yet. It appears once the machine has booted."),
    shell: () => {
      if (!fork.instance_id) return empty("No machine yet, so there is nothing to attach to.");
      const host = el("div", { class: "shell" });
      // Mount after the element is in the document: the terminal measures
      // itself, and measuring a detached node gives a useless size.
      queueMicrotask(() => { shell = openShell(fork, host); });
      return el("div", {},
        el("p", { class: "muted", style: "margin-bottom:10px" },
          "The same machine the agents are on. Anything you change here, they see."),
        host);
    },
  };

  let active = "preview";
  const content = el("div", {});

  const show = (name) => {
    if (name === active && content.firstChild) return;
    active = name;
    // Leaving the shell tab closes the session rather than leaving a terminal
    // running on the machine behind a tab nobody is looking at.
    if (name !== "shell" && shell) { shell.close(); shell = null; }
    clear(content);
    content.append(panes[name]());
    for (const button of tabs.children) {
      button.setAttribute("aria-selected", String(button.dataset.pane === name));
    }
  };

  for (const [name, label] of [["preview", "Preview"], ["shell", "Shell"]]) {
    const button = el("button", { role: "tab", "aria-selected": String(name === active), text: label });
    button.dataset.pane = name;
    button.addEventListener("click", () => show(name));
    tabs.append(button);
  }

  body.append(tabs, content);
  show("preview");
  return section("Workspace", body);
}

// openShell attaches a terminal to the sub-task's machine over a websocket.
function openShell(fork, host) {
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
  const socket = new WebSocket(
    `${scheme}://${location.host}/v1/forks/${fork.id}/shell?cols=${term.cols}&rows=${term.rows}`);
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

const ROUTES = [
  [/^#\/$/, () => renderRepos(), "repos"],
  [/^#\/activity$/, (_, params) => renderActivity(params), "activity"],
  [/^#\/new$/, (_, params) => renderNew(params), "repos"],
  [/^#\/repo\/([\w-]+)$/, (m) => renderRepo(m[1]), "repos"],
  [/^#\/task\/([\w-]+)$/, (m) => renderTask(m[1]), "repos"],
  [/^#\/fork\/([\w-]+)$/, (m) => renderFork(m[1]), "repos"],
];

async function route() {
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

live.start();
refreshStatus();
setInterval(refreshStatus, 5000);
route();
