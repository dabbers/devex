// The form builder: the form as a respondent will see it, edited in place.
//
// Everything typed here is data. It is drawn with textContent and form
// controls only -- never markup -- and saved as JSON that the server checks as
// if it had come from anywhere: types, options, lengths, and that every
// condition looks only at a question before it.
"use strict";

(function () {
  const root = document.getElementById("builder");
  if (!root) return;

  const QUESTION_TYPES = [
    ["text", "Short answer"], ["textarea", "Paragraph"], ["email", "Email"], ["number", "Number"],
    ["date", "Date"], ["url", "Web address"], ["choice", "Multiple choice"], ["dropdown", "Dropdown"],
    ["checkbox", "Tick box"],
  ];
  const OPS = [
    ["equals", "is"], ["not_equals", "is not"], ["contains", "contains"],
    ["answered", "is answered"], ["not_answered", "is not answered"],
  ];
  const asks = (type) => QUESTION_TYPES.some(([t]) => t === type);
  const hasOptions = (type) => type === "choice" || type === "dropdown";

  let form;
  try {
    form = JSON.parse(root.dataset.form);
  } catch {
    root.textContent = "This form could not be read.";
    return;
  }
  form.fields = (form.fields || []).map((f) => ({ ...f, options: f.options || [], show_if: f.show_if || [], _new: !form.id }));
  let selected = null; // index into form.fields
  let dirty = false;

  // ------------------------------------------------------------ helpers

  const el = (tag, props = {}, ...children) => {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(props)) {
      if (value === undefined || value === null || value === false) continue;
      if (key === "text") node.textContent = value;
      else if (key === "class") node.className = value;
      else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
      else if (key in node && typeof value !== "string") node[key] = value;
      else node.setAttribute(key, value);
    }
    for (const child of children.flat()) {
      if (child === null || child === undefined || child === false) continue;
      node.append(child instanceof Node ? child : document.createTextNode(String(child)));
    }
    return node;
  };

  const slugify = (s) => s.toLowerCase().normalize("NFKD").replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 50);

  const uniqueKey = (base, except) => {
    base = base || "question";
    let key = base;
    for (let n = 2; form.fields.some((f, i) => i !== except && f.key === key); n++) key = `${base}-${n}`;
    return key;
  };

  const touch = () => {
    dirty = true;
    status.textContent = "Unsaved changes";
    status.className = "status unsaved";
  };

  // Questions before index i: all a condition at i may look at.
  const earlierQuestions = (i) => form.fields.slice(0, i).filter((f) => asks(f.type));

  const labelOf = (key) => {
    const f = form.fields.find((x) => x.key === key);
    return f ? (f.label || key) : key;
  };

  const describe = (c) => {
    const op = (OPS.find(([o]) => o === c.op) || [c.op, c.op])[1];
    return `“${labelOf(c.field)}” ${op}${c.op === "answered" || c.op === "not_answered" ? "" : ` “${c.value || ""}”`}`;
  };

  // ------------------------------------------------------------ editing

  const rename = (index, label) => {
    const field = form.fields[index];
    field.label = label;
    // A question not yet saved takes its key from its label, so answers are
    // stored under a readable name. Once saved the key is fixed: answers
    // already given are filed under it.
    if (field._new && asks(field.type)) {
      const old = field.key;
      field.key = uniqueKey(slugify(label) || "question", index);
      for (const other of form.fields) {
        for (const c of other.show_if) if (c.field === old) c.field = field.key;
      }
    }
  };

  const insert = (type) => {
    const at = selected === null ? form.fields.length : selected + 1;
    const field = { key: "", label: "", type, required: false, help: "", options: [], show_if: [], _new: true };
    if (type === "page") field.label = "";
    if (type === "text-block") field.label = "A heading";
    if (asks(type)) field.label = "New question";
    if (hasOptions(type)) field.options = ["First choice", "Second choice"];
    field.key = uniqueKey(type === "page" ? "page" : type === "text-block" ? "text" : slugify(field.label), -1);
    form.fields.splice(at, 0, field);
    selected = at;
    touch();
    draw();
  };

  const move = (index, by) => {
    const to = index + by;
    if (to < 0 || to >= form.fields.length) return;
    const [field] = form.fields.splice(index, 1);
    form.fields.splice(to, 0, field);
    selected = to;
    // A question moved above something it depended on would break the rule
    // that conditions look back; drop those conditions and say so.
    const dropped = prune();
    touch();
    draw();
    if (dropped) flash(`${dropped} condition${dropped === 1 ? "" : "s"} removed: a condition can only depend on a question above it.`);
  };

  const remove = (index) => {
    const field = form.fields[index];
    const label = field.label || (field.type === "page" ? "this page break" : "this item");
    if (!confirm(`Remove “${label}”?${asks(field.type) && !field._new ? " Answers already given to it are kept in the export." : ""}`)) return;
    form.fields.splice(index, 1);
    selected = null;
    const dropped = prune();
    touch();
    draw();
    if (dropped) flash(`${dropped} condition${dropped === 1 ? "" : "s"} that depended on it ${dropped === 1 ? "was" : "were"} removed.`);
  };

  // prune drops conditions that no longer look at an earlier question.
  const prune = () => {
    let dropped = 0;
    form.fields.forEach((field, i) => {
      const allowed = new Set(earlierQuestions(i).map((f) => f.key));
      const kept = field.show_if.filter((c) => allowed.has(c.field));
      dropped += field.show_if.length - kept.length;
      field.show_if = kept;
    });
    return dropped;
  };

  // ------------------------------------------------------------ drawing

  const preview = (field) => {
    if (field.type === "page") {
      return el("div", { class: "page-break" },
        el("span", { text: "New page" }), field.label ? el("strong", { text: field.label }) : null);
    }
    if (field.type === "text-block") {
      return el("div", {}, el("h3", { text: field.label || "Heading" }), field.help ? el("p", { class: "block-text", text: field.help }) : null);
    }
    const title = [field.label || "Untitled question", field.required ? el("span", { class: "req", text: " *" }) : null];
    let control;
    if (field.type === "textarea") control = el("textarea", { disabled: true });
    else if (field.type === "checkbox") return el("label", { class: "tick" }, el("input", { type: "checkbox", disabled: true }), " ", ...title);
    else if (field.type === "choice") {
      return el("fieldset", { class: "choices" }, el("legend", {}, ...title),
        ...field.options.map((o) => el("label", { class: "tick" }, el("input", { type: "radio", disabled: true }), " ", o)),
        field.help ? el("p", { class: "help", text: field.help }) : null);
    } else if (field.type === "dropdown") control = el("select", { disabled: true }, el("option", { text: "Choose…" }));
    else control = el("input", { type: ["email", "number", "date", "url"].includes(field.type) ? field.type : "text", disabled: true });
    return el("div", {}, el("label", {}, ...title), control, field.help ? el("p", { class: "help", text: field.help }) : null);
  };

  const conditionEditor = (field, index) => {
    const earlier = earlierQuestions(index);
    const box = el("div", { class: "conditions" });
    const title = field.type === "page" ? "Show this page only if" : "Show this only if";
    if (!earlier.length) {
      box.append(el("p", { class: "help", text: "Conditions can depend on questions above this one. There are none yet." }));
      return box;
    }
    box.append(el("p", { class: "help", text: field.show_if.length ? `${title} all of these hold:` : `${title}… (always shown)` }));
    field.show_if.forEach((c, ci) => {
      const target = form.fields.find((f) => f.key === c.field) || earlier[0];
      const which = el("select", { onchange: () => { c.field = which.value; c.value = ""; touch(); draw(); } },
        ...earlier.map((f) => el("option", { value: f.key, text: f.label || f.key, selected: f.key === c.field })));
      const op = el("select", { onchange: () => { c.op = op.value; touch(); draw(); } },
        ...OPS.map(([o, words]) => el("option", { value: o, text: words, selected: o === c.op })));
      let value = null;
      if (c.op !== "answered" && c.op !== "not_answered") {
        if (hasOptions(target.type)) {
          value = el("select", { onchange: () => { c.value = value.value; touch(); refreshPreview(); } },
            el("option", { value: "", text: "Choose…" }),
            ...target.options.map((o) => el("option", { value: o, text: o, selected: o === c.value })));
        } else if (target.type === "checkbox") {
          c.value = c.value || "yes";
          value = el("select", { onchange: () => { c.value = value.value; touch(); refreshPreview(); } },
            el("option", { value: "yes", text: "ticked", selected: c.value === "yes" }));
        } else {
          value = el("input", { type: "text", value: c.value || "", oninput: () => { c.value = value.value; touch(); refreshPreview(); } });
        }
      }
      box.append(el("div", { class: "condition-row" }, which, op, value,
        el("button", { type: "button", class: "link", text: "Remove", onclick: () => { field.show_if.splice(ci, 1); touch(); draw(); } })));
    });
    box.append(el("button", {
      type: "button", class: "link", text: "+ Add a condition",
      onclick: () => { field.show_if.push({ field: earlier[earlier.length - 1].key, op: "equals", value: "" }); touch(); draw(); },
    }));
    return box;
  };

  const editor = (field, index) => {
    const panel = el("div", { class: "editor", onclick: (e) => e.stopPropagation() });
    const row = (label, control) => el("div", { class: "editor-row" }, el("label", { text: label }), control);

    {
      const labelInput = el("input", {
        type: "text", value: field.label || "", maxLength: 300,
        placeholder: field.type === "page" ? "Page title (optional)" : "",
        oninput: () => { rename(index, labelInput.value); touch(); refreshPreview(); },
      });
      panel.append(row(field.type === "page" ? "Page title" : field.type === "text-block" ? "Heading" : "Question", labelInput));
      setTimeout(() => labelInput.focus(), 0);
    }
    const help = el("textarea", {
      rows: field.type === "text-block" ? 4 : 2, maxLength: 2000,
      oninput: () => { field.help = help.value; touch(); refreshPreview(); },
    });
    help.value = field.help || "";
    panel.append(row(field.type === "text-block" ? "Text" : field.type === "page" ? "Introduction" : "Help text", help));

    if (asks(field.type)) {
      const type = el("select", {
        onchange: () => {
          field.type = type.value;
          if (hasOptions(field.type) && !field.options.length) field.options = ["First choice", "Second choice"];
          touch(); draw();
        },
      }, ...QUESTION_TYPES.map(([t, words]) => el("option", { value: t, text: words, selected: t === field.type })));
      const required = el("input", { type: "checkbox", checked: field.required, onchange: () => { field.required = required.checked; touch(); refreshPreview(); } });
      panel.append(el("div", { class: "editor-row inline" }, el("label", {}, "Type ", type), el("label", { class: "tick" }, required, " Required")));
      if (hasOptions(field.type)) {
        const options = el("textarea", {
          rows: Math.min(8, field.options.length + 1),
          oninput: () => { field.options = options.value.split("\n").map((s) => s.trim()).filter(Boolean); touch(); refreshPreview(); },
        });
        options.value = field.options.join("\n");
        panel.append(row("Choices, one per line", options));
      }
    }
    panel.append(conditionEditor(field, index));
    panel.append(el("div", { class: "editor-actions" },
      el("button", { type: "button", class: "link", text: "↑ Move up", onclick: () => move(index, -1), disabled: index === 0 }),
      el("button", { type: "button", class: "link", text: "↓ Move down", onclick: () => move(index, 1), disabled: index === form.fields.length - 1 }),
      el("button", { type: "button", class: "link danger", text: "Remove", onclick: () => remove(index) }),
      el("button", { type: "button", class: "link", text: "Done", onclick: () => { selected = null; draw(); } })));
    return panel;
  };

  let refreshPreview = () => {};

  const canvas = el("div", { class: "canvas" });
  const status = el("span", { class: "status" });
  const message = el("div", { class: "note", hidden: true, role: "alert" });
  const flash = (text) => { message.textContent = text; message.hidden = false; };

  const titleInput = el("input", {
    type: "text", class: "title-input", placeholder: "Form title", value: form.title || "", maxLength: 300,
    oninput: () => { form.title = titleInput.value; touch(); },
  });
  const description = el("textarea", {
    class: "description-input", rows: 2, placeholder: "A line under the title (optional)", maxLength: 4000,
    oninput: () => { form.description = description.value; touch(); },
  });
  description.value = form.description || "";

  let audience = null;
  if (!form.id) {
    audience = el("select", { onchange: () => { form.audience = audience.value; touch(); } },
      el("option", { value: "internal", text: "Members only (signed in)", selected: form.audience !== "public" }),
      el("option", { value: "public", text: "Anyone with the link (no sign-in)", selected: form.audience === "public" }));
  }

  const draw = () => {
    canvas.replaceChildren();
    let page = 1;
    form.fields.forEach((field, index) => {
      if (field.type === "page") page += 1;
      const body = el("div", { class: "item-preview" });
      const card = el("div", {
        class: `card${index === selected ? " selected" : ""}${field.type === "page" ? " is-page" : ""}`,
        tabIndex: 0,
        onclick: () => { if (selected !== index) { selected = index; draw(); } },
        onkeydown: (e) => { if (e.key === "Enter" && e.target === card) { selected = index; draw(); } },
      }, body);
      const fill = () => {
        body.replaceChildren(preview(field));
        if (field.type === "page") body.firstChild.append(el("span", { class: "muted", text: ` · page ${page}` }));
        if (field.show_if.length) body.append(el("p", { class: "shown-if", text: `Shown only if ${field.show_if.map(describe).join(" and ")}` }));
      };
      fill();
      if (index === selected) {
        refreshPreview = fill;
        card.append(editor(field, index));
      }
      canvas.append(card);
    });
    if (!form.fields.length) canvas.append(el("p", { class: "empty", text: "Add a question to begin." }));
  };

  const save = async () => {
    message.hidden = true;
    const body = {
      id: form.id || "", title: form.title || "", description: form.description || "",
      audience: form.audience || "internal",
      fields: form.fields.map((f) => ({
        key: f.key, label: f.label || "", type: f.type, required: Boolean(f.required),
        help: f.help || "", options: hasOptions(f.type) ? f.options : [], show_if: f.show_if,
      })),
    };
    saveButton.disabled = true;
    status.textContent = "Saving…";
    try {
      const res = await fetch("/forms/save", {
        method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body),
      });
      const answer = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(answer.error || `It could not be saved (${res.status}).`);
      dirty = false;
      if (!form.id) {
        location.replace(`/forms/${encodeURIComponent(answer.id)}/edit`);
        return;
      }
      form.fields = answer.fields.map((f) => ({ ...f, options: f.options || [], show_if: f.show_if || [] }));
      status.textContent = "Saved";
      status.className = "status saved";
      draw();
    } catch (err) {
      flash(err.message);
      status.textContent = "Not saved";
      status.className = "status unsaved";
    } finally {
      saveButton.disabled = false;
    }
  };

  const saveButton = el("button", { type: "button", text: form.id ? "Save" : "Create form", onclick: save });

  root.append(
    el("div", { class: "builder-head" }, titleInput, description,
      audience ? el("label", { class: "audience" }, "Who can answer ", audience) : null),
    message,
    canvas,
    el("div", { class: "toolbar" },
      el("span", { class: "muted", text: "Add" }),
      el("button", { type: "button", class: "secondary", text: "Question", onclick: () => insert("text") }),
      el("button", { type: "button", class: "secondary", text: "Heading and text", onclick: () => insert("text-block") }),
      el("button", { type: "button", class: "secondary", text: "Page break", onclick: () => insert("page") }),
      el("span", { class: "spacer" }),
      status,
      saveButton));

  window.addEventListener("beforeunload", (e) => { if (dirty) { e.preventDefault(); e.returnValue = ""; } });
  draw();
})();
