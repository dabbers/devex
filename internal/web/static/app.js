// dabberz control plane.
//
// Everything rendered here is untrusted: agent output, verifier reports and
// commit messages all flow into the activity feed. Nothing is ever assigned to
// innerHTML; the `el` helper builds nodes and sets text through textContent,
// so a workstream named "<img onerror=...>" is displayed, not executed.

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

// States are grouped by what they mean for the reader, not by name: is this
// working, waiting on the machine, waiting on me, or done.
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
  // Task states share the scale.
  draft: "idle",
  planning: "busy",
  awaiting_plan: "wait",
  running: "busy",
  completed: "good",
  cancelled: "idle",
};

const stateLabel = (state) => String(state || "").replace(/_/g, " ");

const badge = (state, extra = "") =>
  el("span", { class: `badge ${STATE_TONE[state] || "idle"} ${extra}`.trim(), text: stateLabel(state) });

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
    ? at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })
    : "";
};

const money = (usd) => `$${(usd || 0).toFixed(2)}`;
const count = (n) => (n || 0).toLocaleString();

function duration(ns) {
  const seconds = Math.round((ns || 0) / 1e9);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  return `${Math.floor(minutes / 60)}h${minutes % 60 ? ` ${minutes % 60}m` : ""}`;
}

// A meter reads as spent when nothing is left, low under a quarter. The point
// is to show a fork approaching its tripwire before it stops.
function meter(label, used, limit, format = (v) => String(v)) {
  if (!limit) return null;
  const ratio = Math.max(0, Math.min(1, used / limit));
  const tone = ratio >= 1 ? "spent" : ratio > 0.75 ? "low" : "";
  // The number matters more than the bar: "3/8 cycles" says how close this
  // fork is to its tripwire, which a bare bar does not.
  return el("span", { class: `meter ${tone}`.trim(), title: `${label}: ${format(used)} of ${format(limit)}` },
    el("span", { class: "track" }, el("span", { class: "fill", style: `width:${ratio * 100}%` })),
    el("span", { class: "value", text: `${format(used)}/${format(limit)} ${label}` }));
}

function toast(message, bad = false) {
  const node = document.getElementById("toast");
  node.textContent = message;
  node.className = bad ? "toast bad" : "toast";
  node.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { node.hidden = true; }, bad ? 6000 : 3000);
}

// ---------------------------------------------------------------- components

// forkRow renders one fork with enough context to be read on its own: which
// repo and task it belongs to, what it is doing, and how much budget is left.
function forkRow(view) {
  const fork = view.fork;
  const remaining = view.budget_remaining || {};
  const usage = fork.usage || {};

  const meters = [
    meter("cycles", usage.cycles || 0, (usage.cycles || 0) + (remaining.cycles || 0)),
    meter("spend", usage.cost_usd || 0, (usage.cost_usd || 0) + (remaining.cost_usd || 0), money),
  ].filter(Boolean);

  return el("a", { class: "fork", href: `#/fork/${fork.id}` },
    el("div", { class: "fork-head" },
      el("span", { class: "fork-name", text: fork.name }),
      badge(fork.state),
      el("span", { class: "fork-ctx" },
        view.repo_name || "",
        view.task_title ? el("span", { class: "sep", text: "›" }) : null,
        view.task_title || "")),
    el("div", { class: "fork-meta" },
      fork.preview_url ? el("span", { text: "preview ready" }) : null,
      view.waiting_for ? el("span", { text: view.waiting_for }) : null,
      ...meters,
      el("span", { class: "muted", text: relativeTime(fork.updated_at) })));
}

// eventRow renders one audit entry. Structured data is available but folded
// away, so the feed stays readable while remaining complete.
function eventRow(event, isNew = false) {
  const where = [];
  if (event.fork_id) where.push(el("a", { href: `#/fork/${event.fork_id}`, text: "fork" }));
  if (event.task_id) where.push(el("a", { href: `#/task/${event.task_id}`, text: "task" }));
  if (event.repo_id) where.push(el("a", { href: `#/repo/${event.repo_id}`, text: "repo" }));

  const linked = [];
  where.forEach((node, index) => {
    if (index) linked.push(" · ");
    linked.push(node);
  });

  return el("div", { class: `event${isNew ? " new" : ""}` },
    el("time", { datetime: event.created_at, title: event.created_at, text: clockTime(event.created_at) }),
    el("span", { class: `actor ${event.actor === "user" ? "user" : ""}`.trim(), text: event.actor || "system" }),
    el("div", { class: "body" },
      el("div", { class: "msg" },
        el("span", { class: "type", text: event.type }),
        event.message || ""),
      linked.length ? el("div", { class: "where" }, ...linked) : null,
      event.data && Object.keys(event.data).length
        ? el("details", { class: "data" },
            el("summary", { text: "details" }),
            el("pre", { text: JSON.stringify(event.data, null, 2) }))
        : null));
}

// ---------------------------------------------------------------- live stream

// Live keeps one EventSource open and hands every subscriber the events it
// asked for. The cursor is the sequence number, so a reconnect resumes exactly
// where it left off rather than replaying or skipping.
const live = {
  source: null,
  cursor: 0,
  listeners: new Set(),
  retry: 1000,

  start() {
    this.connect();
    // A tab that comes back after sleeping reconnects immediately rather than
    // waiting out the backoff.
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && (!this.source || this.source.readyState === 2)) this.connect();
    });
  },

  connect() {
    if (this.source) this.source.close();
    setConnection("connecting");

    const source = new EventSource(`/v1/events/stream?after=${this.cursor}`);
    this.source = source;

    source.onopen = () => {
      this.retry = 1000;
      setConnection("live");
    };
    source.onmessage = (message) => {
      let event;
      try {
        event = JSON.parse(message.data);
      } catch {
        return;
      }
      if (event.seq > this.cursor) this.cursor = event.seq;
      for (const listener of this.listeners) listener(event);
    };
    source.onerror = () => {
      setConnection("down");
      source.close();
      // Back off, but keep trying: the daemon restarting is a normal event.
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

    const machine = capacity.machine;
    if (machine) {
      const used = machine.used || {};
      const total = machine.total || {};
      strip.append(el("span", {}, "vcpu ", el("b", { text: `${used.vcpus || 0}/${total.vcpus || 0}` })));
      strip.append(el("span", {}, "mem ", el("b", { text: `${Math.round((used.memory_mib || 0) / 1024)}/${Math.round((total.memory_mib || 0) / 1024)}G` })));
    }
    strip.append(el("span", {}, "running ", el("b", { text: count(capacity.active_forks) })));
    if (capacity.queued_forks) {
      strip.append(el("span", {}, "queued ", el("b", { text: count(capacity.queued_forks) })));
    }
    const verification = capacity.verification;
    if (verification) {
      strip.append(el("span", {}, "verify ", el("b", { text: `${verification.held}/${verification.capacity}` }),
        verification.waiting ? ` +${verification.waiting} queued` : ""));
    }
  } catch (err) {
    // The status strip is decoration; a failure here must not blank the page.
    console.warn("status unavailable", err);
  }
}

// ---------------------------------------------------------------- views

const view = () => document.getElementById("view");

function pageHead(title, crumbs = [], ...trailing) {
  const crumb = el("div", { class: "crumb" });
  crumbs.forEach((entry, index) => {
    if (index) crumb.append(" › ");
    crumb.append(entry.href ? el("a", { href: entry.href, text: entry.label }) : entry.label);
  });
  return el("div", { class: "page-head" },
    crumbs.length ? crumb : null,
    el("h1", { text: title }),
    trailing.length ? el("div", { class: "row" }, ...trailing) : null);
}

function section(title, countLabel, ...body) {
  return el("section", { class: "section" },
    el("h2", {}, title, countLabel !== null && countLabel !== undefined
      ? el("span", { class: "count", text: String(countLabel) }) : null),
    ...body);
}

const emptyPanel = (message) => el("div", { class: "panel" }, el("div", { class: "empty", text: message }));

// Overview: everything in flight across every repo, with whatever needs the
// user first.
async function renderOverview() {
  const data = await api.get("/v1/overview");
  const root = view();
  clear(root);

  root.append(pageHead("Overview"));

  if (data.needs_attention?.length) {
    root.append(section("Needs you", data.needs_attention.length,
      el("div", { class: "panel" }, ...data.needs_attention.map(forkRow))));
  }

  const attention = new Set((data.needs_attention || []).map((view) => view.fork.id));
  const running = (data.work || []).filter((view) => !attention.has(view.fork.id));
  root.append(section("In flight", running.length,
    running.length
      ? el("div", { class: "panel" }, ...running.map(forkRow))
      : emptyPanel(attention.size ? "Everything else is finished." : "Nothing running. Start a task from a repo.")));

  root.append(section("Repos", data.repos?.length || 0,
    data.repos?.length
      ? el("div", { class: "grid cards" }, ...data.repos.map(repoCard))
      : emptyPanel("No repos yet.")));

  // A short tail of the unified trail, as a way into the full feed.
  const recent = await api.get("/v1/audit?limit=12");
  const feed = el("div", { class: "panel" },
    ...(recent.events?.length ? recent.events.map((event) => eventRow(event)) : [el("div", { class: "empty", text: "No activity yet." })]));
  root.append(section("Recent activity", null,
    feed,
    el("div", { class: "row-actions" }, el("a", { class: "card", href: "#/activity", text: "Open the full audit trail →" }))));

  // Any event at all can change this page, so it simply refreshes, throttled.
  return live.subscribe(throttle(() => { if (currentRoute() === "#/") renderOverview(); }, 2500));
}

function repoCard(summary) {
  const repo = summary.repo;
  return el("a", { class: "card", href: `#/repo/${repo.id}` },
    el("h3", { text: repo.name }),
    el("div", { class: "sub", text: repo.remote_url }),
    el("div", { class: "row" },
      summary.running ? el("span", { class: "badge busy", text: `${summary.running} running` }) : null,
      summary.queued ? el("span", { class: "badge idle", text: `${summary.queued} queued` }) : null,
      summary.escalated ? el("span", { class: "badge warn", text: `${summary.escalated} needs you` }) : null,
      !repo.discovered ? el("span", { class: "badge plain", text: "not yet discovered" }) : null,
      summary.last_activity ? el("span", { class: "muted", text: relativeTime(summary.last_activity) }) : null));
}

// Activity: the unified audit trail, filterable but always the same log.
async function renderActivity(params) {
  const root = view();
  clear(root);

  const state = {
    repo: params.get("repo") || "",
    actor: params.get("actor") || "",
    follow: params.get("follow") !== "0",
  };

  root.append(pageHead("Activity", [], el("span", { class: "muted", text: "Every action, across every repo." })));

  const repos = await api.get("/v1/repos").catch(() => ({ repos: [] }));
  const feed = el("div", { class: "panel" });

  const repoSelect = el("select", { onchange: (e) => { state.repo = e.target.value; reload(); } },
    el("option", { value: "", text: "All repos" }),
    ...(repos.repos || []).map((repo) =>
      el("option", { value: repo.id, text: repo.name, selected: repo.id === state.repo })));

  const actorSelect = el("select", { onchange: (e) => { state.actor = e.target.value; reload(); } },
    el("option", { value: "", text: "All actors" }));

  const followBox = el("input", {
    type: "checkbox",
    checked: state.follow,
    onchange: (e) => { state.follow = e.target.checked; },
  });

  root.append(el("div", { class: "filters" },
    repoSelect,
    actorSelect,
    el("label", { class: "toggle" }, followBox, "Follow live"),
    el("button", { onclick: () => reload(), text: "Refresh" })));
  root.append(feed);

  const more = el("div", { class: "row-actions" });
  root.append(more);

  let oldestSeq = 0;

  async function reload() {
    const query = new URLSearchParams({ limit: "100" });
    if (state.repo) query.set("repo", state.repo);
    if (state.actor) query.set("actor", state.actor);

    const data = await api.get(`/v1/audit?${query}`);
    clear(feed);
    clear(more);

    if (!data.events?.length) {
      feed.append(el("div", { class: "empty", text: "No activity matches this filter." }));
    } else {
      for (const event of data.events) feed.append(eventRow(event));
      oldestSeq = data.events[data.events.length - 1].seq;
      if (data.events.length >= 100) {
        more.append(el("button", { onclick: loadOlder, text: "Load older" }));
      }
    }

    // The actor list comes from the server so the filter cannot drift from
    // the actors that actually exist.
    if (actorSelect.options.length === 1 && data.actors) {
      for (const actor of data.actors) {
        actorSelect.append(el("option", { value: actor, text: actor, selected: actor === state.actor }));
      }
    }
  }

  async function loadOlder() {
    const query = new URLSearchParams({ limit: "100", before: String(oldestSeq) });
    if (state.repo) query.set("repo", state.repo);
    if (state.actor) query.set("actor", state.actor);
    const data = await api.get(`/v1/audit?${query}`);
    clear(more);
    if (!data.events?.length) {
      more.append(el("span", { class: "muted", text: "Beginning of the trail." }));
      return;
    }
    for (const event of data.events) feed.append(eventRow(event));
    oldestSeq = data.events[data.events.length - 1].seq;
    more.append(el("button", { onclick: loadOlder, text: "Load older" }));
  }

  await reload();

  // New events are prepended in place, so following does not reorder or
  // reload what is already on screen.
  return live.subscribe((event) => {
    if (!state.follow) return;
    if (state.repo && event.repo_id !== state.repo) return;
    if (state.actor && event.actor !== state.actor) return;
    const placeholder = feed.querySelector(".empty");
    if (placeholder) placeholder.remove();
    feed.prepend(eventRow(event, true));
  });
}

async function renderRepos() {
  const data = await api.get("/v1/overview");
  const root = view();
  clear(root);
  root.append(pageHead("Repos"));
  root.append(data.repos?.length
    ? el("div", { class: "grid cards" }, ...data.repos.map(repoCard))
    : emptyPanel("No repos yet. Add one with: dabberzctl repos add <name> <remote-url>"));
}

async function renderRepo(repoID) {
  const [repo, projects, secrets, tasks] = await Promise.all([
    api.get(`/v1/repos/${repoID}`),
    api.get(`/v1/repos/${repoID}/projects`).catch(() => ({ projects: [] })),
    api.get(`/v1/repos/${repoID}/secrets`).catch(() => ({ secrets: [] })),
    api.get(`/v1/tasks?repo=${repoID}`).catch(() => ({ tasks: [] })),
  ]);

  const root = view();
  clear(root);
  root.append(pageHead(repo.name, [{ label: "Repos", href: "#/repos" }],
    el("span", { class: "mono muted", text: repo.remote_url }),
    badge(repo.discovered ? "completed" : "draft", "plain")));

  root.append(section("Projects", projects.projects?.length || 0,
    projects.projects?.length
      ? el("div", { class: "panel" }, ...projects.projects.map((project) =>
          el("div", { class: "workstream" },
            el("h4", { text: project.name }),
            el("p", { class: "mono", text: project.path }),
            el("div", { class: "row" },
              project.toolchain ? el("span", { class: "badge plain", text: project.toolchain }) : null,
              project.confirmed ? el("span", { class: "badge good", text: "confirmed" })
                                : el("span", { class: "badge warn", text: "unconfirmed" })))))
      : emptyPanel("No projects discovered yet.")));

  root.append(section("Tasks", tasks.tasks?.length || 0,
    tasks.tasks?.length
      ? el("div", { class: "panel" }, ...tasks.tasks.map((task) =>
          el("a", { class: "fork", href: `#/task/${task.id}` },
            el("div", { class: "fork-head" },
              el("span", { class: "fork-name", text: task.title }),
              badge(task.state)),
            el("div", { class: "fork-meta" },
              el("span", { text: `merges to ${stateLabel(task.merge_target)}` }),
              el("span", { text: `${stateLabel(task.merge_timing)} timing` }),
              el("span", { class: "muted", text: relativeTime(task.updated_at) })))))
      : emptyPanel("No tasks yet.")));

  // Names only: the control plane never shows a secret's value.
  root.append(section("Secrets", secrets.secrets?.length || 0,
    el("div", { class: "panel pad" },
      secrets.secrets?.length
        ? el("div", { class: "row" }, ...secrets.secrets.map((name) =>
            el("span", { class: "badge plain mono", text: name })))
        : el("span", { class: "muted", text: "No secrets set for this repo." }),
      el("p", { class: "muted", style: "margin:10px 0 0;font-size:12px",
        text: "Inherited by every fork under this repo. Values are never displayed." }))));

  root.append(section("Activity", null,
    await feedPanel(`/v1/audit?repo=${repoID}&limit=40`)));
}

async function feedPanel(path) {
  const data = await api.get(path);
  return el("div", { class: "panel" },
    ...(data.events?.length
      ? data.events.map((event) => eventRow(event))
      : [el("div", { class: "empty", text: "No activity yet." })]));
}

async function renderTask(taskID) {
  const data = await api.get(`/v1/tasks/${taskID}`);
  const task = data.task;
  const root = view();
  clear(root);

  root.append(pageHead(task.title, [
    { label: "Repos", href: "#/repos" },
    { label: "Task", href: `#/task/${task.id}` },
  ], badge(task.state),
    el("span", { class: "muted", text: `merges to ${stateLabel(task.merge_target)}, ${stateLabel(task.merge_timing)}` })));

  root.append(el("div", { class: "panel pad", style: "margin-bottom:18px" },
    el("div", { class: "muted", style: "font-size:12px;margin-bottom:4px", text: "Request" }),
    el("div", { text: task.request })));

  const plan = task.plan;
  if (plan) {
    const open = (plan.questions || []).filter((q) => !q.answer);

    root.append(section(`Plan (round ${plan.round})`, plan.workstreams?.length || 0,
      el("div", { class: "panel" },
        plan.summary ? el("div", { class: "pad", style: "border-bottom:1px solid var(--line)" },
          el("span", { text: plan.summary })) : null,
        ...(plan.workstreams || []).map((workstream) =>
          el("div", { class: "workstream" },
            el("h4", { text: workstream.name }),
            el("p", { text: workstream.description }),
            workstream.serialize_group
              ? el("div", { class: "group" },
                  `runs one at a time with the "${workstream.serialize_group}" group`,
                  workstream.overlap_rationale ? ` — ${workstream.overlap_rationale}` : "")
              : null)))));

    if (open.length) {
      root.append(section("Questions before starting", open.length, questionPanel(task, open)));
    } else if (task.state === "awaiting_plan") {
      root.append(el("div", { class: "callout" },
        el("h3", { text: "Ready to start" }),
        el("p", { text: `Approving starts ${plan.workstreams?.length || 0} workstream(s), each on its own machine.` }),
        el("button", {
          class: "primary",
          text: "Approve and start",
          onclick: async (e) => {
            e.target.disabled = true;
            try {
              await api.send("POST", `/v1/tasks/${task.id}/approve`);
              toast("Plan approved; workstreams queued.");
              renderTask(taskID);
            } catch (err) {
              toast(err.message, true);
              e.target.disabled = false;
            }
          },
        })));
    }
  }

  root.append(section("Workstreams", data.forks?.length || 0,
    data.forks?.length
      ? el("div", { class: "panel" }, ...data.forks.map((fork) =>
          forkRow({ fork, task_title: "", repo_name: "" })))
      : emptyPanel("No workstreams yet.")));

  if (!task.state.match(/completed|cancelled|failed/)) {
    root.append(el("div", { class: "row-actions" },
      el("button", {
        text: "Cancel task",
        onclick: async () => {
          if (!confirm("Cancel this task? Unfinished workstreams are abandoned; their VMs are left in place.")) return;
          try {
            await api.send("POST", `/v1/tasks/${task.id}/cancel`, { reason: "cancelled from the control plane" });
            toast("Task cancelled.");
            renderTask(taskID);
          } catch (err) {
            toast(err.message, true);
          }
        },
      })));
  }

  root.append(section("Activity", null, await feedPanel(`/v1/audit?task=${taskID}&limit=60`)));

  return live.subscribe(throttle((event) => {
    if (event.task_id === taskID && currentRoute().startsWith("#/task/")) renderTask(taskID);
  }, 3000));
}

function questionPanel(task, questions) {
  const panel = el("div", { class: "panel" });
  const answers = {};

  for (const question of questions) {
    const input = el("textarea", {
      placeholder: "Your answer",
      oninput: (e) => { answers[question.id] = e.target.value; },
    });
    const options = el("div", { class: "options" },
      ...(question.options || []).map((option) =>
        el("button", {
          text: option,
          onclick: () => { input.value = option; answers[question.id] = option; },
        })));

    panel.append(el("div", { class: "question" },
      el("p", { text: question.text }),
      question.options?.length ? options : null,
      input));
  }

  panel.append(el("div", { class: "pad" },
    el("button", {
      class: "primary",
      text: "Send answers and replan",
      onclick: async (e) => {
        const filled = Object.fromEntries(Object.entries(answers).filter(([, v]) => v && v.trim()));
        if (!Object.keys(filled).length) {
          toast("Answer at least one question first.", true);
          return;
        }
        e.target.disabled = true;
        try {
          await api.send("POST", `/v1/tasks/${task.id}/answers`, { answers: filled });
          toast("Answers sent; replanning.");
          renderTask(task.id);
        } catch (err) {
          toast(err.message, true);
          e.target.disabled = false;
        }
      },
    })));
  return panel;
}

async function renderFork(forkID) {
  const data = await api.get(`/v1/forks/${forkID}`);
  const fork = data.fork;
  const usage = fork.usage || {};
  const remaining = data.budget_remaining || {};

  const root = view();
  clear(root);
  root.append(pageHead(fork.name, [
    { label: "Overview", href: "#/" },
    { label: "Task", href: `#/task/${fork.task_id}` },
  ], badge(fork.state), fork.state_reason ? el("span", { class: "muted", text: fork.state_reason }) : null));

  if (fork.escalation && fork.state === "escalated") {
    root.append(escalationPanel(fork));
  }

  root.append(el("div", { class: "panel pad", style: "margin-bottom:18px" },
    el("dl", { class: "kv" },
      el("dt", { text: "Branch" }), el("dd", { class: "mono", text: fork.branch }),
      el("dt", { text: "Machine" }), el("dd", { class: "mono", text: fork.instance_id || "not provisioned" }),
      fork.serialize_group ? el("dt", { text: "Serialized with" }) : null,
      fork.serialize_group ? el("dd", { text: fork.serialize_group }) : null,
      el("dt", { text: "Verify/fix rounds" }),
      el("dd", {}, `${usage.cycles || 0}`, remaining.cycles !== undefined ? el("span", { class: "muted", text: ` (${remaining.cycles} left)` }) : null),
      el("dt", { text: "Tokens" }),
      el("dd", { text: count((usage.input_tokens || 0) + (usage.output_tokens || 0)) }),
      el("dt", { text: "Spend" }),
      el("dd", {}, money(usage.cost_usd), remaining.cost_usd !== undefined ? el("span", { class: "muted", text: ` (${money(remaining.cost_usd)} left)` }) : null),
      el("dt", { text: "Working time" }), el("dd", { text: duration(usage.wall_ns) }))));

  if (fork.preview_url) {
    root.append(section("Live preview", null,
      el("div", {},
        el("div", { class: "row-actions", style: "margin:0 0 8px" },
          el("a", { class: "card", href: fork.preview_url, target: "_blank", rel: "noopener noreferrer",
            style: "display:inline-block;padding:6px 12px", text: `Open ${fork.preview_url} ↗` })),
        // The preview is reached over the network exactly as the verifier and
        // an external user reach it.
        el("iframe", { class: "preview-frame", src: fork.preview_url, loading: "lazy",
          sandbox: "allow-scripts allow-same-origin allow-forms" }))));
  }

  root.append(section("Activity", null, await feedPanel(`/v1/audit?fork=${forkID}&limit=100`)));

  return live.subscribe(throttle((event) => {
    if (event.fork_id === forkID && currentRoute().startsWith("#/fork/")) renderFork(forkID);
  }, 3000));
}

function escalationPanel(fork) {
  const escalation = fork.escalation;
  const response = el("textarea", { placeholder: "Tell the agent what to do" });

  const resumeSelect = el("select", {},
    ...["coding", "fixing", "verifying", "merging"].map((state) =>
      el("option", { value: state, text: `resume at ${state}` })));

  return el("div", { class: `callout ${escalation.kind === "tripwire" ? "bad" : ""}`.trim() },
    el("h3", { text: `Waiting for you — ${stateLabel(escalation.kind)}` }),
    el("p", { text: escalation.message }),
    escalation.detail ? el("pre", { text: escalation.detail }) : null,
    response,
    el("div", { class: "row-actions" },
      resumeSelect,
      el("button", {
        class: "primary",
        text: "Answer and resume",
        onclick: async (e) => {
          if (!response.value.trim()) {
            toast("Write an answer first.", true);
            return;
          }
          e.target.disabled = true;
          try {
            await api.send("POST", `/v1/forks/${fork.id}/resolve`, {
              response: response.value.trim(),
              resume: resumeSelect.value,
            });
            toast("Fork resumed.");
            renderFork(fork.id);
          } catch (err) {
            toast(err.message, true);
            e.target.disabled = false;
          }
        },
      })));
}

// ---------------------------------------------------------------- routing

const currentRoute = () => location.hash || "#/";

let disposeView = null;

const ROUTES = [
  [/^#\/$/, () => renderOverview(), "overview"],
  [/^#\/activity/, (_, params) => renderActivity(params), "activity"],
  [/^#\/repos$/, () => renderRepos(), "repos"],
  [/^#\/repo\/([\w-]+)$/, (m) => renderRepo(m[1]), "repos"],
  [/^#\/task\/([\w-]+)$/, (m) => renderTask(m[1]), "overview"],
  [/^#\/fork\/([\w-]+)$/, (m) => renderFork(m[1]), "overview"],
];

async function route() {
  // Each view owns a live subscription; dropping it on navigation is what
  // stops a background page re-rendering over the one being read.
  if (disposeView) {
    disposeView();
    disposeView = null;
  }

  const [path, queryString = ""] = currentRoute().split("?");
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
      root.append(el("div", { class: "panel pad" },
        el("p", { text: err.message }),
        el("button", { text: "Retry", onclick: () => route() })));
    }
    return;
  }

  const root = view();
  clear(root);
  root.append(pageHead("Not found"));
  root.append(el("div", { class: "panel pad" }, el("a", { href: "#/", text: "Back to the overview" })));
}

function highlightTab(active) {
  for (const tab of document.querySelectorAll(".tabs a")) {
    tab.classList.toggle("active", tab.dataset.route === active);
  }
}

// throttle runs fn at most once per interval, keeping the trailing call so a
// burst of events still ends in an up-to-date render.
function throttle(fn, interval) {
  let last = 0;
  let timer = null;
  return (...args) => {
    const wait = interval - (Date.now() - last);
    clearTimeout(timer);
    if (wait <= 0) {
      last = Date.now();
      fn(...args);
    } else {
      timer = setTimeout(() => {
        last = Date.now();
        fn(...args);
      }, wait);
    }
  };
}

window.addEventListener("hashchange", route);

live.start();
refreshStatus();
setInterval(refreshStatus, 5000);
route();
