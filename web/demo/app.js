// Drives the browser demo. The engine itself is fidespipe.wasm: this file only
// loads it, renders what it reports, and hands gate decisions back. Nothing
// here computes a pipeline result — if a number appears on the page, the Go
// engine produced it.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  const el = {
    status: $("engine-status"),
    presets: $("presets"),
    yaml: $("yaml"),
    yamlName: $("yaml-name"),
    validateBtn: $("validate-btn"),
    verdict: $("verdict"),
    terminal: $("terminal"),
    gatePanel: $("gate-panel"),
    gateStep: $("gate-step"),
    gateForm: $("gate-form"),
    gateActions: $("gate-actions"),
    journalPanel: $("journal-panel"),
    journal: $("journal"),
    journalMeta: $("journal-meta"),
    files: $("files"),
    runBtn: $("run-btn"),
    resetBtn: $("reset-btn"),
    runNote: $("run-note"),
    navVersion: $("nav-version"),
    footVersion: $("foot-version"),
  };

  let presets = [];
  let selected = null;
  let running = false;
  let activeGate = null;

  // ---------------------------------------------------------------- status

  function setStatus(text, kind) {
    el.status.className = "ws-status" + (kind ? " " + kind : "");
    el.status.innerHTML = '<span class="dot"></span>' + escapeHtml(text);
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, (c) => (
      { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]
    ));
  }

  // -------------------------------------------------------------- terminal

  // The engine writes its own stdout through the vfs shim. Classification is
  // by the marker the engine already prints, so the page never invents a
  // message the engine did not produce.
  function termWrite(line, fd) {
    const span = document.createElement("span");
    if (fd === 2) {
      span.className = "t-stderr";
    } else if (line.startsWith("▶")) {
      span.className = "t-run";
    } else if (line.includes("══")) {
      span.className = "t-gate";
    } else if (line.startsWith("■")) {
      span.className = "t-end";
    } else if (line.trimStart().startsWith("×") || line.trimStart().startsWith("!")) {
      span.className = "t-err";
    } else if (line.trimStart().startsWith("→")) {
      span.className = "t-dim";
    }
    span.textContent = line;
    el.terminal.appendChild(span);
    el.terminal.scrollTop = el.terminal.scrollHeight;
  }

  // ------------------------------------------------------------------ yaml

  function renderYaml(text, name) {
    el.yamlName.textContent = name;
    const lines = text.split("\n").map((raw) => {
      const trimmed = raw.trimStart();
      if (trimmed.startsWith("#")) return '<span class="c">' + escapeHtml(raw) + "</span>";
      let out = escapeHtml(raw);
      out = out.replace(/^(\s*)([a-z_]+)(:)/, '$1<span class="k">$2</span>$3');
      out = out.replace(/:\s+(-?\d+(?:\.\d+)?)\s*$/, ': <span class="n">$1</span>');
      out = out.replace(/(&quot;[^&]*?&quot;)/g, '<span class="s">$1</span>');
      out = out.replace(/^(\s*-\s)(&quot;)/g, '$1$2');
      return out;
    });
    el.yaml.innerHTML = "<code>" + lines.join("\n") + "</code>";
  }

  // ---------------------------------------------------------------- presets

  function renderPresets() {
    el.presets.innerHTML = "";
    for (const p of presets) {
      const li = document.createElement("li");
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "preset";
      btn.dataset.id = p.id;
      btn.setAttribute("aria-current", String(p.id === selected));
      btn.innerHTML =
        '<span class="preset-name">' + escapeHtml(p.title) + "</span>" +
        '<span class="preset-blurb">' + escapeHtml(p.blurb) + "</span>";
      btn.addEventListener("click", () => selectPreset(p.id));
      li.appendChild(btn);
      el.presets.appendChild(li);
    }
  }

  function selectPreset(id) {
    if (running) return;
    selected = id;
    for (const b of el.presets.querySelectorAll(".preset")) {
      b.setAttribute("aria-current", String(b.dataset.id === id));
    }
    const yamlText = fidespipe.presetYAML(id);
    renderYaml(yamlText, id + ".yaml");
    el.verdict.hidden = true;
    resetRunUi();
    validateNow();
  }

  function validateNow() {
    if (!selected) return;
    const res = JSON.parse(fidespipe.validate(selected));
    const errs = res.errors || [];
    const warns = res.warnings || [];
    el.verdict.hidden = false;
    if (res.ok) {
      el.verdict.className = "verdict ok";
      el.verdict.innerHTML = "&#10003; contract holds &mdash; inputs and outputs line up";
      el.runBtn.disabled = false;
    } else {
      el.verdict.className = "verdict bad";
      el.verdict.innerHTML =
        "&#10007; rejected before execution" +
        "<ul>" + errs.map((e) => "<li>" + escapeHtml(e) + "</li>").join("") + "</ul>" +
        (warns.length
          ? "<ul>" + warns.map((w) => "<li>" + escapeHtml(w) + "</li>").join("") + "</ul>"
          : "");
      // The broken preset is the point of the exercise: validation is the
      // result, so there is deliberately nothing to run.
      el.runBtn.disabled = true;
      note("validation is the deliverable for this preset", "bad");
    }
  }

  function note(text, kind) {
    el.runNote.className = "runbar-note" + (kind ? " " + kind : "");
    el.runNote.textContent = text;
  }

  function resetRunUi() {
    el.gatePanel.hidden = true;
    el.journalPanel.hidden = true;
    el.gateForm.innerHTML = "";
    el.gateActions.innerHTML = "";
    el.journal.innerHTML = "";
    el.files.innerHTML = "";
    activeGate = null;
    note("");
  }

  // ------------------------------------------------------------------ gate

  // gate_wait records each value as JSON, so a string arrives quoted. Unwrap
  // for display and for seeding the input; the engine re-checks the type of
  // whatever comes back.
  function displayValue(raw) {
    if (raw === undefined || raw === null) return "\u2014";
    const s = String(raw);
    if (s === "<\u043d\u0435\u0442 \u0434\u0430\u043d\u043d\u044b\u0445>") return "\u2014";
    try {
      const parsed = JSON.parse(s);
      return typeof parsed === "string" ? parsed : JSON.stringify(parsed);
    } catch {
      return s;
    }
  }

  function coerce(type, text) {
    if (type === "number") {
      const n = Number(text);
      return Number.isFinite(n) ? n : text;
    }
    if (type === "boolean") return text === "true";
    return text;
  }

  function renderGate(g) {
    activeGate = g;
    el.gatePanel.hidden = false;
    el.gateStep.textContent = g.step;

    const values = g.values || {};
    const rows = g.form.map((f) => {
      const shown = displayValue(values[f.field]);
      const valueCell = f.editable
        ? '<input class="gate-input" data-field="' + escapeHtml(f.field) +
          '" data-type="' + escapeHtml(f.type || "string") + '" value="' + escapeHtml(shown) + '">'
        : '<span class="gate-value">' + escapeHtml(shown) + "</span>";
      return "<tr><td class='gate-field'>" + escapeHtml(f.field) + "</td>" +
        "<td>" + valueCell + "</td></tr>";
    }).join("");

    el.gateForm.innerHTML =
      "<thead><tr><th>field</th><th>value from the engine</th></tr></thead><tbody>" +
      rows + "</tbody>";

    el.gateActions.innerHTML = "";
    const accept = document.createElement("button");
    accept.type = "button";
    accept.className = "btn btn-accept";
    accept.textContent = "Accept";
    accept.addEventListener("click", () => decide("accept"));

    const reject = document.createElement("button");
    reject.type = "button";
    reject.className = "btn btn-reject";
    reject.textContent = "Reject";
    reject.addEventListener("click", () => decide("reject"));

    const hint = document.createElement("span");
    hint.className = "gate-note";
    hint.textContent = "run_id " + g.run_id + " \u00b7 the goroutine is parked in WaitDecision";

    el.gateActions.append(accept, reject, hint);
    setStatus("gate \u00b7 waiting for you", "busy");
    note("engine parked at core/human_gate");
    accept.focus();
  }

  function decide(action) {
    if (!activeGate) return;
    const edits = {};
    for (const input of el.gateForm.querySelectorAll(".gate-input")) {
      // Keyed by the full field path: that is what the engine looks up.
      edits[input.dataset.field] = coerce(input.dataset.type, input.value);
    }
    const g = activeGate;

    // Settling the UI before the call matters: SendDecision wakes the parked
    // goroutine, and in js/wasm that can run to completion inside this same
    // call, firing onDone synchronously. Anything written after the call would
    // overwrite the finished state with "resuming".
    activeGate = null;
    for (const b of el.gateActions.querySelectorAll("button")) b.disabled = true;
    el.gatePanel.hidden = true;
    setStatus("resuming", "busy");

    if (!fidespipe.decide(g.handle, action, JSON.stringify(edits))) {
      note("decision was not accepted by the gate", "bad");
    }
  }

  // --------------------------------------------------------------- journal

  const EVENT_KIND = {
    run_start: "k-start", item_start: "k-start", step_start: "k-start",
    gate_wait: "k-gate", gate_decision: "k-gate", gate_retry: "k-gate",
    run_end: "k-end", item_end: "k-end", step_end: "k-end",
    step_failed: "k-bad", step_skipped: "k-bad", contract_warning: "k-bad",
  };

  function renderJournal(done) {
    el.journalPanel.hidden = false;
    const events = done.events || [];
    el.journalMeta.textContent =
      (done.run_id || "\u2014") + " \u00b7 " + events.length + " events";

    el.journal.innerHTML = "";
    for (const ev of events) {
      const type = ev.type || "?";
      const row = document.createElement("div");
      row.className = "ev";

      const chip = document.createElement("span");
      chip.className = "ev-type " + (EVENT_KIND[type] || "");
      chip.textContent = type;

      const body = document.createElement("span");
      body.className = "ev-body";
      const rest = {};
      for (const [k, v] of Object.entries(ev)) if (k !== "type" && k !== "ts") rest[k] = v;
      body.innerHTML = Object.entries(rest)
        .map(([k, v]) => "<b>" + escapeHtml(k) + "</b>=" + escapeHtml(
          typeof v === "object" && v !== null ? JSON.stringify(v) : String(v)))
        .join(" ");

      row.append(chip, body);
      el.journal.appendChild(row);
    }

    const files = done.files || [];
    el.files.innerHTML = files.length
      ? files.map((f) => "<span><b>" + escapeHtml(f.path) + "</b> " + f.size + " B</span>").join("")
      : "<span>no files written</span>";
  }

  // ------------------------------------------------------------------- run

  function runNow() {
    if (!selected || running) return;
    running = true;
    el.runBtn.disabled = true;
    el.resetBtn.disabled = false;
    el.validateBtn.disabled = true;
    resetRunUi();
    el.terminal.innerHTML = "";
    setStatus("running \u00b7 " + selected, "busy");

    const handle = fidespipe.startRun(selected, {
      onGate: (raw) => renderGate(JSON.parse(raw)),
      onDone: (raw) => {
        const done = JSON.parse(raw);
        running = false;
        el.validateBtn.disabled = false;
        renderJournal(done);

        if (done.stopped_at === "validate") {
          setStatus("rejected at validation", "error");
          note("never executed \u2014 the contract caught it", "bad");
        } else if (done.ok) {
          setStatus("run finished", "ready");
          note("ok=" + done.steps_ok + " aborted=" + done.aborted + " \u00b7 journal written", "ok");
        } else {
          setStatus("run failed", "error");
          note(done.error || ("aborted=" + done.aborted), "bad");
        }
        el.runBtn.disabled = false;
      },
    });

    if (handle < 0) {
      running = false;
      el.validateBtn.disabled = false;
      setStatus("start failed", "error");
      note("could not start the run", "bad");
      el.runBtn.disabled = false;
    }
  }

  function resetAll() {
    if (running) return;
    el.terminal.innerHTML = "";
    resetRunUi();
    el.resetBtn.disabled = true;
    setStatus("ready", "ready");
    if (selected) validateNow();
  }

  // ------------------------------------------------------------------ boot

  async function loadWasm() {
    const go = new Go();
    go.argv = ["fidespipewasm"];
    go.env = { TMPDIR: "/tmp" };

    const ready = new Promise((resolve) => { globalThis.fidespipeReady = resolve; });

    let instance;
    try {
      const res = await WebAssembly.instantiateStreaming(fetch("fidespipe.wasm"), go.importObject);
      instance = res.instance;
    } catch {
      // Servers that do not label .wasm as application/wasm break
      // instantiateStreaming; compiling the bytes directly does not care.
      const bytes = await (await fetch("fidespipe.wasm")).arrayBuffer();
      instance = (await WebAssembly.instantiate(bytes, go.importObject)).instance;
    }

    // main() blocks forever on purpose so later runs can be started; awaiting
    // this would hang the page.
    go.run(instance).catch(() => {});
    await ready;
  }

  async function boot() {
    fidespipeVFS.setOutputSink(termWrite);

    el.runBtn.addEventListener("click", runNow);
    el.resetBtn.addEventListener("click", resetAll);
    el.validateBtn.addEventListener("click", validateNow);

    try {
      await loadWasm();
    } catch (err) {
      setStatus("failed to load the wasm module", "error");
      note(String(err), "bad");
      return;
    }

    const version = fidespipe.version();
    el.navVersion.textContent = "v" + version;
    el.footVersion.textContent = "fidespipe v" + version;

    presets = JSON.parse(fidespipe.presets());
    setStatus("ready \u00b7 engine loaded in this tab", "ready");
    renderPresets();
    selectPreset(presets[0].id);
    el.resetBtn.disabled = false;
  }

  boot();
})();
