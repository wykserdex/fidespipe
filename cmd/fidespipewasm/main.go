//go:build js && wasm

// Command fidespipewasm runs the real Fidespipe engine inside a browser.
//
// Nothing here re-implements the engine. Presets are parsed by
// pipeline.LoadPipelineFileFromBytes, checked by core.Validate and executed by
// core.Run — the same three calls the CLI makes — and the run journal is
// written to an in-memory filesystem (web/demo/vfs.js) that satisfies the Node
// fs surface Go's js/wasm syscalls call into. journal.Journal holds a concrete
// *os.File, so giving the engine a filesystem leaves the core untouched
// instead of turning the journal into an interface.
//
// Gates are real: RunOptions.GateUI hands the engine a gate.ChannelUI, the run
// parks in WaitDecision, and the browser resolves it when a person clicks.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall/js"
	"time"

	"github.com/wykserdex/fidespipe/internal/buildinfo"
	"github.com/wykserdex/fidespipe/internal/core"
	"github.com/wykserdex/fidespipe/internal/execution"
	"github.com/wykserdex/fidespipe/internal/gate"
	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
)

//go:embed presets/*.yaml
var presetsFS embed.FS

// runsDir lives in the in-memory filesystem the page installs before the
// module is instantiated.
const runsDir = "/fp/var/runs"

// preset pairs an embedded pipeline with the copy the page shows next to it.
// Only builtin plugins (core/text_stats, core/human_gate) are usable here:
// everything else in the registry is a Python subprocess, and js/wasm cannot
// start a process.
type preset struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Blurb string `json:"blurb"`
	File  string `json:"-"`
}

var presets = []preset{
	{
		ID:    "human_gate",
		Title: "Human in the loop",
		Blurb: "Metrics are computed, then the run stops and waits for a person. " +
			"longest_word is editable — overwrite what the engine found and the " +
			"override is recorded in the journal as part of the decision.",
		File: "human_gate.yaml",
	},
	{
		ID:    "foreach",
		Title: "foreach over a batch",
		Blurb: "One step, three items. input.text is rebound per iteration and the " +
			"outputs aggregate into steps.scan_all, which the gate reviews.",
		File: "foreach.yaml",
	},
	{
		ID:    "when",
		Title: "Conditional branch",
		Blurb: "The second step only runs past a word-count threshold. The input is " +
			"three words, so it is skipped and the journal says why.",
		File: "when.yaml",
	},
	{
		ID:    "broken",
		Title: "Contract violation",
		Blurb: "Meant to be validated, not run. input.text is a number where the " +
			"plugin declares a string port — reported before anything executes.",
		File: "broken.yaml",
	},
}

func presetByID(id string) (preset, bool) {
	for _, p := range presets {
		if p.ID == id {
			return p, true
		}
	}
	return preset{}, false
}

// runHandle is one browser-initiated run. The engine goroutine owns the
// lifecycle; the page holds only the integer id.
type runHandle struct {
	mu     sync.Mutex
	id     int
	preset preset
	runID  string
	ui     *gate.ChannelUI
	cancel context.CancelFunc
	done   bool

	onGate js.Value
	onDone js.Value
}

var (
	handlesMu sync.Mutex
	handles   = map[int]*runHandle{}
	nextID    = 1
)

func register(h *runHandle) int {
	handlesMu.Lock()
	defer handlesMu.Unlock()
	h.id = nextID
	nextID++
	handles[h.id] = h
	return h.id
}

func lookup(id int) *runHandle {
	handlesMu.Lock()
	defer handlesMu.Unlock()
	return handles[id]
}

func (h *runHandle) runDir() string { return filepath.Join(runsDir, h.runID) }

func main() {
	api := js.Global().Get("Object").New()
	api.Set("version", js.FuncOf(func(js.Value, []js.Value) any { return buildinfo.Resolve() }))
	api.Set("presets", js.FuncOf(jsPresets))
	api.Set("presetYAML", js.FuncOf(jsPresetYAML))
	api.Set("validate", js.FuncOf(jsValidate))
	api.Set("startRun", js.FuncOf(jsStartRun))
	api.Set("decide", js.FuncOf(jsDecide))
	api.Set("journal", js.FuncOf(jsJournal))
	js.Global().Set("fidespipe", api)

	if ready := js.Global().Get("fidespipeReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}

	// The Go scheduler parks blocked goroutines and yields to the JS event
	// loop, so a run waiting on a gate costs nothing here. Returning from main
	// would exit the module, which must stay alive for later startRun calls.
	select {}
}

func jsPresets(js.Value, []js.Value) any {
	b, err := json.Marshal(presets)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func jsPresetYAML(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return ""
	}
	p, ok := presetByID(args[0].String())
	if !ok {
		return ""
	}
	raw, err := presetsFS.ReadFile("presets/" + p.File)
	if err != nil {
		return ""
	}
	return string(raw)
}

// loadPreset parses and validates one preset. Validation runs against the same
// engine the CLI uses, so a preset that fails here fails identically there.
func loadPreset(id string) (*pipeline.PipelineFile, []string, []string, error) {
	p, ok := presetByID(id)
	if !ok {
		return nil, nil, nil, fmt.Errorf("unknown preset %q", id)
	}
	raw, err := presetsFS.ReadFile("presets/" + p.File)
	if err != nil {
		return nil, nil, nil, err
	}
	pf, err := pipeline.LoadPipelineFileFromBytes(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	errs, warns := core.Validate(pf, core.NewEngine())
	return pf, errs, warns, nil
}

func jsValidate(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return marshal(map[string]any{"ok": false, "errors": []string{"no preset id"}})
	}
	_, errs, warns, err := loadPreset(args[0].String())
	if err != nil {
		return marshal(map[string]any{"ok": false, "errors": []string{err.Error()}})
	}
	return marshal(map[string]any{
		"ok":       len(errs) == 0,
		"errors":   errs,
		"warnings": warns,
	})
}

func jsStartRun(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return -1
	}
	id := args[0].String()
	cbs := args[1]

	p, ok := presetByID(id)
	if !ok {
		return -1
	}
	pf, errs, warns, err := loadPreset(id)
	if err != nil {
		cbs.Get("onDone").Invoke(marshal(map[string]any{
			"ok": false, "error": err.Error(),
		}))
		return -1
	}

	h := &runHandle{preset: p, onGate: cbs.Get("onGate"), onDone: cbs.Get("onDone")}

	// The broken preset exists to be validated, so stop where the CLI stops.
	if len(errs) > 0 {
		register(h)
		h.onDone.Invoke(marshal(map[string]any{
			"ok": false, "preset": p.ID, "stopped_at": "validate",
			"errors": errs, "warnings": warns, "events": []any{},
		}))
		return h.id
	}

	runID, err := execution.NewRunID(pf.Pipeline.Name)
	if err != nil {
		h.onDone.Invoke(marshal(map[string]any{"ok": false, "error": err.Error()}))
		return -1
	}
	h.runID = runID
	hid := register(h)

	go h.run(pf)
	return hid
}

func (h *runHandle) run(pf *pipeline.PipelineFile) {
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()
	defer cancel()

	stats, err := core.Run(pf, core.NewEngine(), core.RunOptions{
		// Yes stays false on purpose: --yes is what lets the CLI demo
		// auto-accept, and auto-accepting here would defeat the only thing
		// this page is trying to show.
		Yes:     false,
		RunsDir: runsDir,
		RunID:   h.runID,
		Ctx:     ctx,
		GateUI:  h.gateFactory,

		// Agent-track policy, applied to the browser for the same reason it
		// exists for MCP: nothing but a person resolves this gate.
		NoAutoApprove: true,
	})

	h.report(stats, err)
}

// gateFactory is called by the runner for every human_gate step. Returning a
// ChannelUI makes the gate take the structured path: the run parks in
// WaitDecision until the page calls decide().
func (h *runHandle) gateFactory(st *pipeline.Step) gate.GateUI {
	ui := gate.NewChannelUI()

	h.mu.Lock()
	h.ui = ui
	h.mu.Unlock()

	go h.notifyGate(st)
	return ui
}

// notifyGate tells the page what the engine is asking about.
//
// It runs in its own goroutine because the runner calls the GateUI factory
// before Service.Run writes the gate_wait event — and that event is the only
// place the engine records the values behind the form. The builtin steps emit
// no output event of their own, so there is nothing earlier to read. Waiting
// for the engine's own record beats resolving the form here: a second
// implementation of form resolution would just be a second thing to be wrong.
//
// The run is parked in WaitDecision throughout, so nothing moves underneath
// this loop.
func (h *runHandle) notifyGate(st *pipeline.Step) {
	deadline := time.Now().Add(3 * time.Second)
	values, ready := h.gateWaitValues(st)
	for !ready && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		values, ready = h.gateWaitValues(st)
	}

	h.onGate.Invoke(marshal(map[string]any{
		"handle": h.id,
		"run_id": h.runID,
		"step":   st.ID,
		"form":   h.formView(st),
		"values": values,
	}))
}

type fieldView struct {
	Field    string `json:"field"`
	Editable bool   `json:"editable"`
	Type     string `json:"type"`
}

func (h *runHandle) formView(st *pipeline.Step) []fieldView {
	out := make([]fieldView, 0, len(st.Form))
	for _, f := range st.Form {
		out = append(out, fieldView{Field: f.Field, Editable: f.Editable, Type: f.Type})
	}
	return out
}

// gateWaitValues returns the values the engine rendered into the gate_wait
// event for this step, and whether that event exists yet.
func (h *runHandle) gateWaitValues(st *pipeline.Step) (map[string]any, bool) {
	values := map[string]any{}
	events, err := journal.NewReader(h.runDir()).Events()
	if err != nil {
		return values, false
	}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if typ, _ := ev["type"].(string); typ != "gate_wait" {
			continue
		}
		if step, _ := ev["step"].(string); step != st.ID {
			continue
		}
		form, _ := ev["form"].([]interface{})
		for _, raw := range form {
			f, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if field, _ := f["field"].(string); field != "" {
				values[field] = f["value"]
			}
		}
		return values, true
	}
	return values, false
}

func jsDecide(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return false
	}
	h := lookup(args[0].Int())
	if h == nil {
		return false
	}
	action := args[1].String()

	// Edits are keyed by FULL field path ("steps.stats.longest_word"), not by
	// basename: gate.validateStructuredEdits looks up edits[f.Field] and skips
	// a key it does not recognise without recording it as skipped. The
	// terminal path uses basenames, the structured path does not.
	edits := map[string]interface{}{}
	if len(args) > 2 && args[2].Type() == js.TypeString && args[2].String() != "" {
		if err := json.Unmarshal([]byte(args[2].String()), &edits); err != nil {
			return false
		}
	}

	h.mu.Lock()
	ui := h.ui
	h.mu.Unlock()
	if ui == nil {
		return false
	}
	return ui.SendDecision(gate.Decision{Action: action, Edits: edits, Source: gate.SourceGUI})
}

func (h *runHandle) report(stats execution.RunStats, runErr error) {
	h.mu.Lock()
	if h.done {
		h.mu.Unlock()
		return
	}
	h.done = true
	h.cancelLocked()
	h.mu.Unlock()

	// stdout is line-buffered on the JS side, so the tail of the engine's
	// output is still held back when the run ends.
	if vfs := js.Global().Get("fidespipeVFS"); vfs.Type() == js.TypeObject {
		if flush := vfs.Get("flushOutput"); flush.Type() == js.TypeFunction {
			flush.Invoke()
		}
	}

	payload := map[string]any{
		"handle":   h.id,
		"preset":   h.preset.ID,
		"run_id":   h.runID,
		"run_dir":  h.runDir(),
		"ok":       runErr == nil && stats.Aborted == 0,
		"steps_ok": stats.OK,
		"aborted":  stats.Aborted,
		"events":   h.events(),
		"files":    h.files(),
	}
	if runErr != nil {
		payload["error"] = runErr.Error()
	}
	h.onDone.Invoke(marshal(payload))
}

func (h *runHandle) cancelLocked() {
	if h.cancel != nil {
		h.cancel()
	}
}

// events reads journal.jsonl back through the engine's own reader. What the
// page displays is the file the run wrote, not a log reconstructed beside it.
func (h *runHandle) events() []map[string]interface{} {
	ev, err := journal.NewReader(h.runDir()).Events()
	if err != nil {
		return []map[string]interface{}{}
	}
	return ev
}

func (h *runHandle) files() []map[string]any {
	var out []map[string]any
	walk(h.runDir(), "", &out)
	return out
}

func walk(dir, rel string, out *[]map[string]any) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		if e.IsDir() {
			walk(filepath.Join(dir, name), childRel, out)
			continue
		}
		info, err := e.Info()
		size := int64(-1)
		if err == nil {
			size = info.Size()
		}
		*out = append(*out, map[string]any{"path": childRel, "size": size})
	}
}

// jsJournal returns the journal of a finished run so the page can re-render it
// after a reload of the panel.
func jsJournal(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return marshal(map[string]any{"events": []any{}})
	}
	h := lookup(args[0].Int())
	if h == nil {
		return marshal(map[string]any{"events": []any{}})
	}
	return marshal(map[string]any{
		"run_id": h.runID,
		"events": h.events(),
		"files":  h.files(),
	})
}

func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"marshal failed"}`
	}
	return string(b)
}
