// Node harness for the browser demo: runs the same wasm module against the
// same in-memory vfs.js the page installs, with no DOM involved. Used to check
// the engine/VFS/gate wiring before opening a browser.
"use strict";

const path = require("path");
const fsNode = require("fs");
const util = require("util");
const GOROOT = process.argv[2];
const WASM = process.argv[3];
const VFS = process.argv[4];
const PRESET = process.argv[5] || "human_gate";
const DECISION = process.argv[6] || "accept";

globalThis.TextEncoder = util.TextEncoder;
globalThis.TextDecoder = util.TextDecoder;
globalThis.performance ??= require("performance");
globalThis.crypto ??= require("crypto");

require(path.join(GOROOT, "lib", "wasm", "wasm_exec.js"));

// vfs.js replaces globalThis.fs/path/process with the in-memory shims. All
// node requires must happen before this point.
require(VFS);

const realExit = process.exit ? process.exit.bind(process) : () => {};
const keepAlive = setInterval(() => {}, 1000);

let output = "";
globalThis.fidespipeVFS.setOutputSink((chunk, fd) => {
  output += chunk;
  realStdoutWrite((fd === 2 ? "[stderr] " : "") + chunk);
});
const realStdoutWrite = (s) => fsNode.writeSync(1, s);

const go = new Go();
go.argv = ["fidespipewasm"];
go.env = { TMPDIR: "/tmp" };

WebAssembly.instantiate(fsNode.readFileSync(WASM), go.importObject)
  .then((result) => go.run(result.instance))
  .catch((err) => { realStdoutWrite("instantiate failed: " + err + "\n"); clearInterval(keepAlive); });

globalThis.fidespipeReady = () => {
  realStdoutWrite("\n=== wasm ready, version " + fidespipe.version() + " ===\n");
  realStdoutWrite("presets: " + fidespipe.presets() + "\n");
  realStdoutWrite("validate(" + PRESET + "): " + fidespipe.validate(PRESET) + "\n\n");

  const handle = fidespipe.startRun(PRESET, {
    onGate: (raw) => {
      const g = JSON.parse(raw);
      realStdoutWrite("\n=== GATE (step " + g.step + ") ===\n");
      realStdoutWrite("form: " + JSON.stringify(g.form) + "\n");
      realStdoutWrite("values: " + JSON.stringify(g.values) + "\n");
      const edits = {};
      for (const f of g.form) {
        if (f.editable) {
          // Keyed by full field path: that is what gate.validateStructuredEdits
          // looks up, and a basename key is dropped without being reported.
          edits[f.field] = "overridden-by-human";
          realStdoutWrite("editing " + f.field + " -> overridden-by-human\n");
        }
      }
      const ok = fidespipe.decide(g.handle, DECISION, JSON.stringify(edits));
      realStdoutWrite("decide(" + DECISION + ") -> " + ok + "\n");
    },
    onDone: (raw) => {
      const d = JSON.parse(raw);
      realStdoutWrite("\n=== DONE ===\n");
      realStdoutWrite("ok=" + d.ok + " run_id=" + d.run_id + " stopped_at=" + (d.stopped_at || "-") + "\n");
      if (d.error) realStdoutWrite("error: " + d.error + "\n");
      if (d.errors) realStdoutWrite("validation errors: " + JSON.stringify(d.errors) + "\n");
      realStdoutWrite("steps_ok=" + d.steps_ok + " aborted=" + d.aborted + "\n");
      realStdoutWrite("files: " + JSON.stringify(d.files) + "\n");
      realStdoutWrite("events (" + (d.events || []).length + "):\n");
      for (const ev of d.events || []) {
        realStdoutWrite("  " + JSON.stringify(ev) + "\n");
      }
      realStdoutWrite("\n=== engine stdout (verbatim) ===\n" + output + "\n");
      clearInterval(keepAlive);
      globalThis.__done = true;
    },
  });
  realStdoutWrite("startRun -> handle " + handle + "\n");
};
