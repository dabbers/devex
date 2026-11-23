// Answering a form: one page at a time, and only the questions its
// conditions show. The server applies the same rules to whatever arrives, so
// this is for the person answering, not for enforcement. Without it the form
// still works: every page shows at once and the server sorts it out.
"use strict";

(function () {
  const form = document.querySelector("form.answering");
  if (!form) return;

  const pages = Array.from(form.querySelectorAll("section.page"));
  const back = form.querySelector("[data-back]");
  const next = form.querySelector("[data-next]");
  const send = form.querySelector("[data-send]");
  const progress = form.querySelector("[data-progress]");
  form.classList.add("paged");

  const conditionsOf = (node) => {
    try {
      const parsed = JSON.parse(node.dataset.showIf || "[]");
      return Array.isArray(parsed) ? parsed : [];
    } catch {
      return [{ op: "never" }]; // unreadable hides, as on the server
    }
  };

  // The value a question holds right now, as the server would read it.
  const valueOf = (item) => {
    const key = item.dataset.key;
    const inputs = item.querySelectorAll(`[name="${CSS.escape(key)}"]`);
    if (!inputs.length) return null; // a text block asks nothing
    for (const input of inputs) {
      if (input.type === "radio" || input.type === "checkbox") {
        if (input.checked) return input.value.trim();
      } else {
        return input.value.trim();
      }
    }
    return "";
  };

  const holds = (c, answers) => {
    const got = answers[c.field] || "";
    const want = (c.value || "");
    switch (c.op) {
      case "equals": return got.toLowerCase() === want.toLowerCase();
      case "not_equals": return got.toLowerCase() !== want.toLowerCase();
      case "contains": return want !== "" && got.toLowerCase().includes(want.toLowerCase());
      case "answered": return got !== "";
      case "not_answered": return got === "";
      default: return false;
    }
  };

  // Walk the form in order, as the server does: a hidden question counts as
  // unanswered for everything after it.
  const visiblePages = () => {
    const answers = {};
    const shown = [];
    for (const page of pages) {
      const pageShown = conditionsOf(page).every((c) => holds(c, answers));
      page.dataset.hidden = pageShown ? "" : "yes";
      if (pageShown) shown.push(page);
      for (const item of page.querySelectorAll(".item")) {
        const show = pageShown && conditionsOf(item).every((c) => holds(c, answers));
        item.hidden = !show;
        // Disabled so a hidden answer is not sent; the value is kept in case
        // the condition holds again.
        for (const input of item.querySelectorAll("input, textarea, select")) input.disabled = !show;
        const value = valueOf(item);
        if (show && value !== null) answers[item.dataset.key] = value;
      }
    }
    return shown;
  };

  let current = 0;

  const missing = (page) => {
    for (const item of page.querySelectorAll(".item")) {
      if (item.hidden || !item.querySelector("[data-required]")) continue;
      if (valueOf(item) === "") return item;
    }
    return null;
  };

  const say = (item) => {
    form.querySelectorAll(".item.has-problem").forEach((n) => n.classList.remove("has-problem"));
    let note = form.querySelector(".page-note");
    if (!item) { if (note) note.remove(); return; }
    item.classList.add("has-problem");
    if (!note) {
      note = document.createElement("div");
      note.className = "note page-note";
      note.setAttribute("role", "alert");
    }
    const label = item.querySelector("label, legend");
    note.textContent = (label ? label.textContent.replace("*", "").trim() : "This question") + " is required.";
    item.before(note);
    const focus = item.querySelector("input, textarea, select");
    if (focus) focus.focus();
  };

  const draw = () => {
    const shown = visiblePages();
    current = Math.min(current, shown.length - 1);
    for (const page of pages) page.hidden = page !== shown[current];
    const last = current === shown.length - 1;
    back.hidden = current === 0;
    next.hidden = last;
    send.hidden = !last;
    progress.textContent = shown.length > 1 ? `Page ${current + 1} of ${shown.length}` : "";
  };

  next.addEventListener("click", () => {
    const shown = visiblePages();
    const problem = missing(shown[current]);
    say(problem);
    if (problem) return;
    current += 1;
    draw();
    window.scrollTo({ top: form.offsetTop - 16 });
  });
  back.addEventListener("click", () => {
    say(null);
    current = Math.max(0, current - 1);
    draw();
    window.scrollTo({ top: form.offsetTop - 16 });
  });
  form.addEventListener("submit", (event) => {
    const shown = visiblePages();
    for (let i = 0; i < shown.length; i++) {
      const problem = missing(shown[i]);
      if (problem) {
        event.preventDefault();
        current = i;
        draw();
        say(problem);
        return;
      }
    }
  });
  form.addEventListener("input", draw);
  form.addEventListener("change", draw);

  // Opened again after the server found a problem: start where it was.
  const start = Number(form.dataset.start || 0);
  const shown = visiblePages();
  const at = shown.indexOf(pages[start]);
  current = at >= 0 ? at : 0;
  draw();
})();
