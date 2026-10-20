package api

// v0.25 (v0.26a: format_version; v0.27: when; v0.28: foreach/parallel;
// v0.29: retry/after_foreach; v0.30: typed inputs and step loops): round-trip
// parse → doc → serialize → YAML обязан читаться
// ядром и проходить валидацию; pos возвращается; unsupported блокирует
// serialize; конфликты управляющего потока — честно в errors.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

func postBytes(t *testing.T, url string, body []byte) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func fileToBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEditorParseSingleCheck(t *testing.T) {
	ts, _ := gateTestServer(t)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", fileToBytes(t, "../../examples/single_check.yaml"))
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	if doc["name"] != "single_check" {
		t.Fatalf("name = %v", doc["name"])
	}
	if doc["format_version"] != "0.2" {
		t.Fatalf("format_version = %v (single_check — 0.2)", doc["format_version"])
	}
	steps, _ := doc["steps"].([]interface{})
	if len(steps) != 3 {
		t.Fatalf("steps = %d", len(steps))
	}
	gate, _ := steps[2].(map[string]interface{})
	if gate["plugin"] != "core/human_gate" {
		t.Fatalf("gate plugin = %v", gate["plugin"])
	}
	actions, _ := gate["actions"].([]interface{})
	if len(actions) != 2 {
		t.Fatalf("actions = %v", gate["actions"])
	}
	form, _ := gate["form"].([]interface{})
	if len(form) != 2 {
		t.Fatalf("form = %v", gate["form"])
	}
	unsup, _ := doc["unsupported"].([]interface{})
	if len(unsup) != 0 {
		t.Fatalf("unsupported = %v (single_check — чистый для редактора)", unsup)
	}
	inputs, _ := doc["input"].([]interface{})
	if len(inputs) != 1 {
		t.Fatalf("input = %v", inputs)
	}
	if inputs[0].(map[string]interface{})["default"] != "user@mailinator.com" {
		t.Fatalf("input default = %v", inputs[0])
	}
}

func TestEditorParsePreservesStepPos(t *testing.T) {
	ts, _ := gateTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: positioned
  steps:
    - id: review
      plugin: core/human_gate
      pos: [17, 23]
      actions: [accept]
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	steps, ok := doc["steps"].([]interface{})
	if !ok || len(steps) != 1 {
		t.Fatalf("steps=%v", doc["steps"])
	}
	step, ok := steps[0].(map[string]interface{})
	if !ok {
		t.Fatalf("step=%v", steps[0])
	}
	pos, ok := step["pos"].([]interface{})
	if !ok || len(pos) != 2 || pos[0] != float64(17) || pos[1] != float64(23) {
		t.Fatalf("pos=%v, want [17, 23]", step["pos"])
	}
}

func TestEditorParseTypedInputAndPreservesOrdinaryMap(t *testing.T) {
	ts, _ := gateTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: typed_inputs
  input:
    count:
      type: number
      required: true
      description: Maximum number of results
    email:
      type: string
      required: false
      default: user@example.test
      format: email
      description: Contact address
    ordinary:
      type: string
      value: payload
  steps:
    - id: review
      plugin: core/human_gate
      when:
        path: input.count
        op: gt
        value: 10
      actions: [accept]
      retry:
        attempts: 3
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("parse code=%d body=%v", code, doc)
	}
	if unsupported, _ := doc["unsupported"].([]interface{}); len(unsupported) != 0 {
		t.Fatalf("typed inputs still block editing: %v", unsupported)
	}
	inputs := make(map[string]map[string]interface{})
	for _, rawInput := range doc["input"].([]interface{}) {
		input := rawInput.(map[string]interface{})
		inputs[input["name"].(string)] = input
	}
	count := inputs["count"]
	if count["typed"] != true || count["type"] != "number" || count["required"] != true || count["has_default"] != false {
		t.Fatalf("typed input count=%v", count)
	}
	email := inputs["email"]
	if email["typed"] != true || email["type"] != "string" || email["required"] != false || email["format"] != "email" || email["description"] != "Contact address" || email["default"] != "user@example.test" || email["has_default"] != true {
		t.Fatalf("typed input email=%v", email)
	}
	ordinary, ok := inputs["ordinary"]["default"].(map[string]interface{})
	if !ok || !reflect.DeepEqual(ordinary, map[string]interface{}{"type": "string", "value": "payload"}) {
		t.Fatalf("ordinary object input=%v", inputs["ordinary"])
	}
	// when/retry остаются редактируемыми и не попадают в unsupported.
	step := doc["steps"].([]interface{})[0].(map[string]interface{})
	if step["when"].(map[string]interface{})["op"] != "gt" {
		t.Fatalf("doc.when=%v", step["when"])
	}
	retry := step["retry"].(map[string]interface{})
	if retry["attempts"] != float64(3) {
		t.Fatalf("doc.retry=%v", retry)
	}

	encoded, _ := json.Marshal(doc)
	code, body := postBytes(t, ts.URL+"/api/serialize/pipeline", encoded)
	if code != 200 {
		t.Fatalf("serialize code=%d body=%v", code, body)
	}
	yamlText := body["yaml"].(string)
	parsed, err := pipeline.LoadPipelineFileFromBytes([]byte(yamlText))
	if err != nil {
		t.Fatalf("serialized YAML не читается: %v\n%s", err, yamlText)
	}
	countDescriptor := parsed.Pipeline.Input["count"].(map[string]interface{})
	if countDescriptor["type"] != "number" || countDescriptor["required"] != true || countDescriptor["description"] != "Maximum number of results" {
		t.Fatalf("round-trip count=%v", countDescriptor)
	}
	emailDescriptor := parsed.Pipeline.Input["email"].(map[string]interface{})
	if emailDescriptor["default"] != "user@example.test" || emailDescriptor["format"] != "email" || emailDescriptor["required"] != false {
		t.Fatalf("round-trip email=%v", emailDescriptor)
	}
	if !reflect.DeepEqual(parsed.Pipeline.Input["ordinary"], map[string]interface{}{"type": "string", "value": "payload"}) {
		t.Fatalf("ordinary map changed on round-trip: %v", parsed.Pipeline.Input["ordinary"])
	}
}

func TestEditorRejectsUnknownInputSchemaType(t *testing.T) {
	ts, _ := gateTestServer(t)
	doc := map[string]interface{}{
		"name":  "bad_input_schema",
		"input": []interface{}{map[string]interface{}{"name": "value", "typed": true, "type": "mystery", "has_default": false}},
		"steps": []interface{}{}, "unsupported": []interface{}{},
	}
	encoded, _ := json.Marshal(doc)
	code, _ := postBytes(t, ts.URL+"/api/serialize/pipeline", encoded)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown input type accepted: status=%d, want %d", code, http.StatusBadRequest)
	}
}

func TestEditorLoopFieldsRoundTrip(t *testing.T) {
	ts, _ := gateTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: loop_editor
  input: {}
  steps:
    - id: repeat
      plugin: core/human_gate
      loop: retry_cycle
      loop_condition: steps.repeat.continue
      max_iterations: 5
      actions: [accept]
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("parse code=%d body=%v", code, doc)
	}
	step := doc["steps"].([]interface{})[0].(map[string]interface{})
	if step["loop"] != "retry_cycle" || step["loop_condition"] != "steps.repeat.continue" || step["max_iterations"] != float64(5) {
		t.Fatalf("parsed loop fields=%v", step)
	}
	encoded, _ := json.Marshal(doc)
	code, body := postBytes(t, ts.URL+"/api/serialize/pipeline", encoded)
	if code != 200 {
		t.Fatalf("serialize code=%d body=%v", code, body)
	}
	parsed, err := pipeline.LoadPipelineFileFromBytes([]byte(body["yaml"].(string)))
	if err != nil {
		t.Fatalf("serialized YAML не читается: %v", err)
	}
	got := parsed.Pipeline.Steps[0]
	if got.Loop != "retry_cycle" || got.LoopCondition != "steps.repeat.continue" || got.MaxIterations != 5 {
		t.Fatalf("serialized loop fields=%+v", got)
	}
}

func TestEditorSerializeRoundTrip(t *testing.T) {
	ts, _ := gateTestServer(t)
	raw := fileToBytes(t, "../../examples/gate_demo.yaml")
	_, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	// назначаем позиции (редактор бы их поставил мышкой)
	for _, st := range doc["steps"].([]interface{}) {
		st.(map[string]interface{})["pos"] = []interface{}{float64(120), float64(80)}
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out["ok"], out["errors"])
	}
	yamlText, _ := out["yaml"].(string)
	if !strings.Contains(yamlText, "pos:") || !strings.Contains(yamlText, "120") {
		t.Fatalf("yaml без pos:\n%s", yamlText)
	}
	// v0.26a: версия исходного файла (0.1) сохраняется, а не подменяется
	if doc["format_version"] != "0.1" || !strings.Contains(yamlText, `format_version: "0.1"`) {
		t.Fatalf("round-trip потерял format_version (doc=%v):\n%s", doc["format_version"], yamlText)
	}
	// сгенерированный YAML обязан читаться ядром
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(yamlText))
	if err != nil {
		t.Fatalf("ядро не читает свой же YAML: %v", err)
	}
	errs, _ := pipeline.Validate(pf, plugin.NewEngine())
	if len(errs) > 0 {
		t.Fatalf("валидация: %v", errs)
	}
	// ре-парсинг: pos вернулся
	_, doc2 := postBytes(t, ts.URL+"/api/parse/pipeline", []byte(yamlText))
	st2 := doc2["steps"].([]interface{})[0].(map[string]interface{})
	pos, _ := st2["pos"].([]interface{})
	if len(pos) != 2 || pos[0] != float64(120) || pos[1] != float64(80) {
		t.Fatalf("pos после round-trip = %v", st2["pos"])
	}
}

func TestEditorWhenParse(t *testing.T) {
	// v0.27: when_demo — when теперь под управлением редактора (parse не
	// требует манифестов — только структура)
	ts, _ := gateTestServer(t)
	raw := fileToBytes(t, "../../examples/when_demo.yaml")
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	unsup, _ := doc["unsupported"].([]interface{})
	if len(unsup) != 0 {
		t.Fatalf("unsupported = %v (when_demo теперь редакторский)", unsup)
	}
	steps := doc["steps"].([]interface{})
	w, _ := steps[1].(map[string]interface{})["when"].(map[string]interface{})
	if w == nil || w["path"] != "steps.stats.words" || w["op"] != "gte" || w["value"] != float64(10) {
		t.Fatalf("when = %v (ждём {steps.stats.words gte 10})", w)
	}
}

func TestEditorWhenRoundTrip(t *testing.T) {
	// v0.27: полный цикл parse → serialize → ядро читает+валидирует → re-parse.
	// Синтетика из core/human_gate: в тестовом окружении нет plugins/ —
	// builtin гейт манифест не требует.
	ts, _ := gateTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: when_test
  input:
    n: 5
  steps:
    - id: a
      plugin: core/human_gate
      form:
        - field: input.n
          editable: true
      actions: [accept]

    - id: b
      plugin: core/human_gate
      when: { path: steps.a.n, op: gte, value: 10 }
      form:
        - field: input.n
          editable: false
      actions: [accept]
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out["ok"], out["errors"])
	}
	yamlText, _ := out["yaml"].(string)
	if !strings.Contains(yamlText, "when:") || !strings.Contains(yamlText, "steps.a.n") ||
		!strings.Contains(yamlText, "op: gte") || !strings.Contains(yamlText, "value: 10") {
		t.Fatalf("yaml без when:\n%s", yamlText)
	}
	// ядро читает и валидирует сгенерированный YAML (when-проверки валидатора
	// проходят: шаги в порядке, op из WhenOps)
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(yamlText))
	if err != nil {
		t.Fatalf("ядро не читает свой же YAML: %v", err)
	}
	errs, _ := pipeline.Validate(pf, plugin.NewEngine())
	if len(errs) > 0 {
		t.Fatalf("валидация: %v", errs)
	}
	// re-parse: when вернулся с теми же параметрами
	_, doc2 := postBytes(t, ts.URL+"/api/parse/pipeline", []byte(yamlText))
	w2, _ := doc2["steps"].([]interface{})[1].(map[string]interface{})["when"].(map[string]interface{})
	if w2 == nil || w2["path"] != "steps.a.n" || w2["op"] != "gte" || w2["value"] != float64(10) {
		t.Fatalf("when после round-trip = %v", w2)
	}
}

func TestEditorWhenTruthyAndCoerce(t *testing.T) {
	// v0.27: новое doc с when — truthy без value; и коэрсия строки «5» в число
	// для gt (UI шлёт value текстом)
	ts, _ := gateTestServer(t)
	doc := map[string]interface{}{
		"name":  "when_fresh",
		"input": []interface{}{map[string]interface{}{"name": "n", "default": "1"}},
		"steps": []interface{}{
			map[string]interface{}{
				"id": "a", "plugin": "core/human_gate", "pos": []interface{}{0.0, 0.0},
				"on_error": "stop", "form": []interface{}{}, "actions": []interface{}{"accept"},
				"on_reject": "stop",
				"when":      map[string]interface{}{"path": "input.n", "op": "truthy"},
			},
			map[string]interface{}{
				"id": "b", "plugin": "core/human_gate", "pos": []interface{}{0.0, 40.0},
				"on_error": "stop", "form": []interface{}{}, "actions": []interface{}{"accept"},
				"on_reject": "stop",
				"when":      map[string]interface{}{"path": "input.n", "op": "gt", "value": "5"},
			},
		},
		"unsupported": []interface{}{},
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out["ok"], out["errors"])
	}
	yamlText, _ := out["yaml"].(string)
	if !strings.Contains(yamlText, "op: truthy") {
		t.Fatalf("truthy потерялся:\n%s", yamlText)
	}
	// gt: «5» (строка из UI) обязано стать 5 (число) — иначе ядро на рантайме
	// не сможет сравнить
	if strings.Contains(yamlText, `value: "5"`) || !strings.Contains(yamlText, "value: 5") {
		t.Fatalf("коэрсия «5» → 5 не сработала:\n%s", yamlText)
	}
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(yamlText))
	if err != nil {
		t.Fatalf("ядро не читает: %v", err)
	}
	errs, _ := pipeline.Validate(pf, plugin.NewEngine())
	if len(errs) > 0 {
		t.Fatalf("валидация: %v", errs)
	}
}

// ── v0.28: foreach / parallel_group / after_foreach ─────────────────────────

// flowFixturePlugin — минимальный манифест (без бинаря: валидация манифест
// читает, исполнять не нужно).
const flowFixturePlugin = `id: test-fixture
version: 0.1.0
platform_api: "^0.1"
runtime:
  type: python
  entry: main.py
  requires: []
input:
  item:
    from: input.item
    type: string
    format: text
output:
  done: { type: boolean }
permissions:
  network: []
  filesystem: none
  secrets: []
`

// flowTestServer — gateTestServer + фикстурный плагин (для валидации
// step-foreach/parallel: в tempdir нет plugins/).
func flowTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	pipelines := filepath.Join(dir, "pipelines")
	runs := filepath.Join(dir, "runs")
	for _, d := range []string{plugins, pipelines, runs} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(plugins, "test-fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugins, "test-fixture", "plugin.yaml"), []byte(flowFixturePlugin), 0644); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(plugins, pipelines, runs)
	srv.Engine.PluginsDir = plugins // реестровые имена (без plugins/) → tempdir
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, runs
}

func TestEditorForeachParse(t *testing.T) {
	// v0.28: реальные примеры — parse вытаскивает управляющий поток,
	// unsupported пуст (все три файла редакторские)
	ts, _ := gateTestServer(t)

	raw := fileToBytes(t, "../../examples/foreach_step_demo.yaml")
	_, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if unsup, _ := doc["unsupported"].([]interface{}); len(unsup) != 0 {
		t.Fatalf("foreach_step_demo: unsupported = %v", unsup)
	}
	fx := doc["steps"].([]interface{})[0].(map[string]interface{})
	if fx["foreach"] != "input.texts" || fx["foreach_item"] != "text" {
		t.Fatalf("freqs = %v (ждём foreach input.texts, item text)", fx)
	}

	raw = fileToBytes(t, "../../examples/parallel_demo.yaml")
	_, doc = postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if unsup, _ := doc["unsupported"].([]interface{}); len(unsup) != 0 {
		t.Fatalf("parallel_demo: unsupported = %v (обычный object input редактируем)", unsup)
	}
	w := doc["steps"].([]interface{})[0].(map[string]interface{})
	if w["parallel_group"] != "analyze" {
		t.Fatalf("words.parallel_group = %v", w["parallel_group"])
	}

	raw = fileToBytes(t, "../../examples/csv_foreach_summary.yaml")
	_, doc = postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if unsup, _ := doc["unsupported"].([]interface{}); len(unsup) != 0 {
		t.Fatalf("csv_foreach_summary: unsupported = %v", unsup)
	}
	if doc["foreach"] != "steps.load.rows" || doc["foreach_item"] != "row" || doc["item_type"] != "object" {
		t.Fatalf("pipeline-foreach = %v/%v/%v", doc["foreach"], doc["foreach_item"], doc["item_type"])
	}
	last := doc["steps"].([]interface{})
	sum := last[len(last)-1].(map[string]interface{})
	if !sum["after_foreach"].(bool) {
		t.Fatalf("summary.after_foreach = %v (ждём true)", sum["after_foreach"])
	}
}

func TestEditorForeachPipelineRoundTrip(t *testing.T) {
	// v0.28: pipeline-foreach + after_foreach на гейтах — полный цикл:
	// parse → serialize → ядро читает+валидирует → re-parse (гейты — builtin,
	// манифесты не нужны)
	ts, _ := gateTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: foreach_test
  input:
    list: ["a", "b"]
  foreach: input.list
  foreach_item: row
  item_type: string
  steps:
    - id: per
      plugin: core/human_gate
      form:
        - field: input.row
          editable: false
      actions: [accept]

    - id: sum
      plugin: core/human_gate
      after_foreach: true
      form:
        - field: input.row
          editable: false
      actions: [accept]
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out["ok"], out["errors"])
	}
	yamlText, _ := out["yaml"].(string)
	for _, want := range []string{"foreach: input.list", "foreach_item: row", "item_type: string", "after_foreach: true"} {
		if !strings.Contains(yamlText, want) {
			t.Fatalf("yaml без %q:\n%s", want, yamlText)
		}
	}
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(yamlText))
	if err != nil {
		t.Fatalf("ядро не читает свой же YAML: %v", err)
	}
	errs, _ := pipeline.Validate(pf, plugin.NewEngine())
	if len(errs) > 0 {
		t.Fatalf("валидация: %v", errs)
	}
	// re-parse: всё вернулось
	_, doc2 := postBytes(t, ts.URL+"/api/parse/pipeline", []byte(yamlText))
	if doc2["foreach"] != "input.list" || doc2["foreach_item"] != "row" || doc2["item_type"] != "string" {
		t.Fatalf("pipeline-foreach после round-trip: %v", doc2)
	}
	sum2 := doc2["steps"].([]interface{})[1].(map[string]interface{})
	if !sum2["after_foreach"].(bool) {
		t.Fatalf("after_foreach после round-trip: %v", sum2)
	}
}

func TestEditorStepForeachRoundTrip(t *testing.T) {
	// v0.28: step-foreach + parallel_group на фикстурном плагине — полный
	// цикл с валидацией (существует ли плагин, пути, конфликты)
	ts, runs := flowTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: step_foreach_test
  input:
    texts: ["a", "b"]
    x: "hi"
  steps:
    - id: fx
      plugin: test-fixture
      foreach: input.texts
      foreach_item: item
      on_error: stop

    - id: par
      plugin: test-fixture
      parallel_group: g1
      on_error: stop
      bind:
        item: input.x
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	if unsup, _ := doc["unsupported"].([]interface{}); len(unsup) != 0 {
		t.Fatalf("unsupported = %v", unsup)
	}
	fx := doc["steps"].([]interface{})[0].(map[string]interface{})
	if fx["foreach"] != "input.texts" || fx["foreach_item"] != "item" {
		t.Fatalf("fx = %v", fx)
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out["ok"], out["errors"])
	}
	yamlText, _ := out["yaml"].(string)
	for _, want := range []string{"foreach: input.texts", "foreach_item: item", "parallel_group: g1"} {
		if !strings.Contains(yamlText, want) {
			t.Fatalf("yaml без %q:\n%s", want, yamlText)
		}
	}
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(yamlText))
	if err != nil {
		t.Fatalf("ядро не читает: %v", err)
	}
	eng := plugin.NewEngine()
	eng.PluginsDir = filepath.Join(filepath.Dir(runs), "plugins")
	errs, _ := pipeline.Validate(pf, eng)
	if len(errs) > 0 {
		t.Fatalf("валидация: %v", errs)
	}
}

func TestEditorForeachConflicts(t *testing.T) {
	// v0.28: конфликты валидатора проходят через редактор честно:
	// foreach + after_foreach и foreach + parallel_group → ok:false c ошибкой
	ts, _ := flowTestServer(t)
	mk := func(extra map[string]interface{}) {
		t.Helper()
		doc := map[string]interface{}{
			"name": "conflict", "input": []interface{}{map[string]interface{}{"name": "texts", "default": "a"}},
			"steps": []interface{}{map[string]interface{}{
				"id": "fx", "plugin": "test-fixture", "pos": []interface{}{0.0, 0.0},
				"on_error": "stop", "bind": map[string]interface{}{},
				"form": []interface{}{}, "actions": []interface{}{}, "on_reject": "",
				"foreach": "input.texts",
			}},
			"unsupported": []interface{}{},
		}
		step := doc["steps"].([]interface{})[0].(map[string]interface{})
		for k, v := range extra {
			step[k] = v
		}
		d, _ := json.Marshal(doc)
		code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
		if code != 200 {
			t.Fatalf("code=%d body=%v", code, out)
		}
		if out["ok"] != false {
			t.Fatalf("конфликт %v не отловлен: ok=%v", extra, out)
		}
	}
	mk(map[string]interface{}{"after_foreach": true})
	mk(map[string]interface{}{"parallel_group": "g1"})
}

// ── v0.29: retry ───────────────────────────────────────────────────────────

func TestEditorRetryParse(t *testing.T) {
	// v0.29: llm_same_provider — теперь полностью редакторский (retry под
	// управлением, type-объявлений в input нет)
	ts, _ := gateTestServer(t)
	raw := fileToBytes(t, "../../examples/llm_same_provider.yaml")
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	if unsup, _ := doc["unsupported"].([]interface{}); len(unsup) != 0 {
		t.Fatalf("unsupported = %v (llm_same_provider теперь редакторский)", unsup)
	}
	draft := doc["steps"].([]interface{})[0].(map[string]interface{})
	if draft["on_error"] != "retry" {
		t.Fatalf("draft.on_error = %v (ждём retry)", draft["on_error"])
	}
	r, _ := draft["retry"].(map[string]interface{})
	if r == nil || r["attempts"] != float64(3) || r["delay"] != "2s" || r["backoff"] != "exponential" {
		t.Fatalf("draft.retry = %v (ждём {3, 2s, exponential})", r)
	}
}

func TestEditorRetryRoundTrip(t *testing.T) {
	// v0.29: on_error: retry + retry-блок на фикстурном плагине — полный цикл
	// с валидацией
	ts, runs := flowTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: retry_test
  input:
    x: "hi"
  steps:
    - id: fx
      plugin: test-fixture
      on_error: retry
      retry: { attempts: 3, delay: 5s, backoff: exponential }
      bind:
        item: input.x
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	fx := doc["steps"].([]interface{})[0].(map[string]interface{})
	if fx["on_error"] != "retry" {
		t.Fatalf("on_error = %v", fx["on_error"])
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out["ok"], out["errors"])
	}
	yamlText, _ := out["yaml"].(string)
	for _, want := range []string{"on_error: retry", "attempts: 3", "delay: 5s", "backoff: exponential"} {
		if !strings.Contains(yamlText, want) {
			t.Fatalf("yaml без %q:\n%s", want, yamlText)
		}
	}
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(yamlText))
	if err != nil {
		t.Fatalf("ядро не читает: %v", err)
	}
	eng := plugin.NewEngine()
	eng.PluginsDir = filepath.Join(filepath.Dir(runs), "plugins")
	errs, _ := pipeline.Validate(pf, eng)
	if len(errs) > 0 {
		t.Fatalf("валидация: %v", errs)
	}
	// re-parse: retry вернулся
	_, doc2 := postBytes(t, ts.URL+"/api/parse/pipeline", []byte(yamlText))
	r2, _ := doc2["steps"].([]interface{})[0].(map[string]interface{})["retry"].(map[string]interface{})
	if r2 == nil || r2["attempts"] != float64(3) || r2["delay"] != "5s" || r2["backoff"] != "exponential" {
		t.Fatalf("retry после round-trip = %v", r2)
	}
}

func TestEditorRetryValidation(t *testing.T) {
	// v0.29: честность валидатора: on_error=retry + attempts < 1 → ok:false
	ts, _ := flowTestServer(t)
	doc := map[string]interface{}{
		"name": "retry_bad", "input": []interface{}{map[string]interface{}{"name": "x", "default": "hi"}},
		"steps": []interface{}{map[string]interface{}{
			"id": "fx", "plugin": "test-fixture", "pos": []interface{}{0.0, 0.0},
			"on_error": "retry", "bind": map[string]interface{}{"item": "input.x"},
			"form": []interface{}{}, "actions": []interface{}{}, "on_reject": "",
			"retry": map[string]interface{}{"attempts": 0, "delay": "1s", "backoff": "fixed"},
		}},
		"unsupported": []interface{}{},
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != false {
		t.Fatalf("attempts=0 не отловлен: ok=%v", out)
	}
}

func TestEditorSerializeNewGatePipeline(t *testing.T) {
	ts, _ := gateTestServer(t)
	doc := map[string]interface{}{
		"name":  "fresh",
		"input": []interface{}{map[string]interface{}{"name": "note", "default": "hi"}},
		"steps": []interface{}{
			map[string]interface{}{
				"id": "review", "plugin": "core/human_gate", "pos": []interface{}{0.0, 0.0},
				"on_error":  "stop",
				"form":      []interface{}{map[string]interface{}{"field": "input.note", "editable": true}},
				"actions":   []interface{}{"accept", "reject"},
				"on_reject": "stop",
			},
		},
		"unsupported": []interface{}{},
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out["ok"], out["errors"])
	}
	yamlText, _ := out["yaml"].(string)
	if !strings.Contains(yamlText, "core/human_gate") || !strings.Contains(yamlText, "on_reject: stop") {
		t.Fatalf("yaml:\n%s", yamlText)
	}
	// v0.26a: новое doc без версии → текущая 0.2
	if !strings.Contains(yamlText, `format_version: "0.2"`) {
		t.Fatalf("новому doc не присвоено 0.2:\n%s", yamlText)
	}
}

// ── v0.5: secrets в редакторе ──────────────────────────────────────────────

// secretFixturePlugin — манифест, запрашивающий env-ключ (кросс-чек v0.17:
// pipeline.secrets ↔ permissions.secrets).
const secretFixturePlugin = `id: secret-fixture
version: 0.1.0
platform_api: "^0.1"
runtime:
  type: python
  entry: main.py
  requires: []
input:
  item:
    from: input.item
    type: string
    format: text
output:
  done: { type: boolean }
permissions:
  network: []
  filesystem: none
  secrets: [SECRET_FIXTURE_KEY]
`

// secretTestServer — flowTestServer + фикстурный плагин с секретом.
func secretTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv, runs := flowTestServer(t)
	plugins := filepath.Join(filepath.Dir(runs), "plugins")
	if err := os.MkdirAll(filepath.Join(plugins, "secret-fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugins, "secret-fixture", "plugin.yaml"), []byte(secretFixturePlugin), 0644); err != nil {
		t.Fatal(err)
	}
	return srv, runs
}

func TestEditorSecretsParse(t *testing.T) {
	// v0.5: pipeline.secrets под управлением — llm_same_provider редакторский,
	// secrets в doc (а не в unsupported)
	ts, _ := gateTestServer(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "llm_same_provider.yaml"))
	if err != nil {
		t.Fatalf("пример не читается: %v", err)
	}
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	sers, _ := doc["secrets"].([]interface{})
	if len(sers) != 1 || sers[0] != "GEMINI_API_KEY" {
		t.Fatalf("doc.secrets = %v (ждём [GEMINI_API_KEY])", sers)
	}
	for _, u := range doc["unsupported"].([]interface{}) {
		if u == "pipeline.secrets" {
			t.Fatalf("pipeline.secrets в unsupported — v0.5 управляет секретами: %v", doc["unsupported"])
		}
	}
}

func TestEditorSecretsRoundTrip(t *testing.T) {
	// v0.5: плагин просит SECRET_FIXTURE_KEY, пайплайн его объявляет —
	// round-trip сохраняет secrets, ложных warnings нет
	ts, _ := secretTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: secrets_test
  input:
    x: "hi"
  secrets: [SECRET_FIXTURE_KEY]
  steps:
    - id: fx
      plugin: secret-fixture
      bind:
        item: input.x
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	sers, _ := doc["secrets"].([]interface{})
	if len(sers) != 1 || sers[0] != "SECRET_FIXTURE_KEY" {
		t.Fatalf("doc.secrets = %v", sers)
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out["ok"], out["errors"])
	}
	yamlText, _ := out["yaml"].(string)
	if !strings.Contains(yamlText, "secrets:") || !strings.Contains(yamlText, "SECRET_FIXTURE_KEY") {
		t.Fatalf("yaml без secrets:\n%s", yamlText)
	}
	// кросс-чек: плагин запрашивает ключ, пайплайн объявил — warning не ложный
	warns, _ := out["warnings"].([]interface{})
	for _, w := range warns {
		if ws, ok := w.(string); ok && strings.Contains(ws, "плагину нужен ключ") {
			t.Fatalf("ложный warning о не объявленном ключе: %v", warns)
		}
	}
	// re-parse: secrets вернулся
	_, doc2 := postBytes(t, ts.URL+"/api/parse/pipeline", []byte(yamlText))
	sers2, _ := doc2["secrets"].([]interface{})
	if len(sers2) != 1 || sers2[0] != "SECRET_FIXTURE_KEY" {
		t.Fatalf("secrets после round-trip = %v", sers2)
	}
}

func TestEditorSecretsWarnings(t *testing.T) {
	// v0.5: плагин просит ключ, пайплайн НЕ объявляет → warning (ядро:
	// предупреждение, не ошибка — ok остаётся true, решит пользователь)
	ts, _ := secretTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: secrets_missing
  input:
    x: "hi"
  steps:
    - id: fx
      plugin: secret-fixture
      bind:
        item: input.x
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v (недообъявленный секрет — warning, не error) errors=%v", out["ok"], out["errors"])
	}
	warns, _ := out["warnings"].([]interface{})
	found := false
	for _, w := range warns {
		if ws, ok := w.(string); ok && strings.Contains(ws, "SECRET_FIXTURE_KEY") {
			found = true
		}
	}
	if !found {
		t.Fatalf("нет warning о ключе SECRET_FIXTURE_KEY: %v", warns)
	}
}

// ── v0.6: network (платформа allow/deny) в редакторе ──────────────────────

// netFixturePlugin — манифест, заявляющий сеть (declare-now).
const netFixturePlugin = `id: net-fixture
version: 0.1.0
platform_api: "^0.1"
runtime:
  type: python
  entry: main.py
  requires: []
input:
  item:
    from: input.item
    type: string
    format: text
output:
  done: { type: boolean }
permissions:
  network:
    - { host: example.com, port: 443 }
  filesystem: none
  secrets: []
`

// netAnyFixturePlugin — манифест с явным any_host: единственное сетевое
// объявление, которое песочница умеет исполнить, поэтому network: allow
// с ним валиден.
const netAnyFixturePlugin = `id: net-any-fixture
version: 0.1.0
platform_api: "^0.1"
runtime:
  type: python
  entry: main.py
  requires: []
input:
  item:
    from: input.item
    type: string
    format: text
output:
  done: { type: boolean }
permissions:
  network:
    - { any_host: true, port: 443 }
  filesystem: none
  secrets: []
`

// netTestServer — flowTestServer + фикстурный плагин, заявляющий сеть.
func netTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv, runs := flowTestServer(t)
	plugins := filepath.Join(filepath.Dir(runs), "plugins")
	if err := os.MkdirAll(filepath.Join(plugins, "net-fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugins, "net-fixture", "plugin.yaml"), []byte(netFixturePlugin), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(plugins, "net-any-fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugins, "net-any-fixture", "plugin.yaml"), []byte(netAnyFixturePlugin), 0644); err != nil {
		t.Fatal(err)
	}
	return srv, runs
}

func TestEditorNetworkParse(t *testing.T) {
	// v0.6: pipeline.network под управлением — text_stats (офлайн-пайплайн)
	// отдаёт network: deny в doc, а не в unsupported
	ts, _ := gateTestServer(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "text_stats.yaml"))
	if err != nil {
		t.Fatalf("пример не читается: %v", err)
	}
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	if doc["network"] != "deny" {
		t.Fatalf("doc.network = %v (ждём \"deny\")", doc["network"])
	}
	for _, u := range doc["unsupported"].([]interface{}) {
		if u == "pipeline.network" {
			t.Fatalf("pipeline.network в unsupported — v0.6 управляет сетью: %v", doc["unsupported"])
		}
	}
}

func TestEditorNetworkRoundTrip(t *testing.T) {
	// v0.6: deny round-trip'ится; allow (пусто) — поле в YAML не прописывается
	ts, _ := flowTestServer(t)
	// 1) deny: test-fixture сеть НЕ заявляет → валидация ok
	raw := []byte(`format_version: "0.2"
pipeline:
  name: net_deny
  network: deny
  input:
    x: "hi"
  steps:
    - id: fx
      plugin: test-fixture
      bind:
        item: input.x
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	if doc["network"] != "deny" {
		t.Fatalf("doc.network = %v", doc["network"])
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out["ok"], out["errors"])
	}
	yamlText, _ := out["yaml"].(string)
	if !strings.Contains(yamlText, "network: deny") {
		t.Fatalf("yaml без network: deny:\n%s", yamlText)
	}
	_, doc2 := postBytes(t, ts.URL+"/api/parse/pipeline", []byte(yamlText))
	if doc2["network"] != "deny" {
		t.Fatalf("network после round-trip = %v", doc2["network"])
	}
	// 2) allow (network пуст): поле в YAML не появляется (omitempty)
	raw2 := []byte(`format_version: "0.2"
pipeline:
  name: net_allow
  input:
    x: "hi"
  steps:
    - id: fx
      plugin: test-fixture
      bind:
        item: input.x
`)
	_, doc3 := postBytes(t, ts.URL+"/api/parse/pipeline", raw2)
	if doc3["network"] != "" {
		t.Fatalf("doc.network = %v (ждём пусто = allow)", doc3["network"])
	}
	d3, _ := json.Marshal(doc3)
	_, out3 := postBytes(t, ts.URL+"/api/serialize/pipeline", d3)
	if out3["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out3["ok"], out3["errors"])
	}
	y3, _ := out3["yaml"].(string)
	if strings.Contains(y3, "network:") {
		t.Fatalf("allow: field network: прописан в YAML:\n%s", y3)
	}
}

func TestEditorNetworkDenyConflict(t *testing.T) {
	// v0.6: кросс-чек ядра — плагин заявил сеть + network: deny = ОШИБКА
	// (declare-now: честный сетевой плагин не спрятается за «пайплайн не просил»)
	ts, _ := netTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: net_conflict
  network: deny
  input:
    x: "hi"
  steps:
    - id: fx
      plugin: net-fixture
      bind:
        item: input.x
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, doc)
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["ok"] == true {
		t.Fatalf("ок=TRUE при плагине с сетью + deny — кросс-чек сломан")
	}
	joined := ""
	for _, e := range out["errors"].([]interface{}) {
		joined += e.(string) + " "
	}
	if !strings.Contains(joined, "network: deny") {
		t.Fatalf("ошибка без network: deny: %s", joined)
	}
	// allow + список host:port — тоже ошибка: точечный фильтр не реализован,
	// поэтому выдать полный доступ вместо списка нельзя. Валидатор обязан
	// повторять отказ рантайма, а не пропускать пайплайн зелёным.
	rawAllowHost := []byte(`format_version: "0.2"
pipeline:
  name: net_allow_host
  network: allow
  input:
    x: "hi"
  steps:
    - id: fx
      plugin: net-fixture
      bind:
        item: input.x
`)
	_, docA := postBytes(t, ts.URL+"/api/parse/pipeline", rawAllowHost)
	dA, _ := json.Marshal(docA)
	code, outA := postBytes(t, ts.URL+"/api/serialize/pipeline", dA)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, outA)
	}
	if outA["ok"] != false {
		t.Fatalf("allow + host:port принят, но рантайм его отвергнет: %v", outA)
	}
	joinedA := ""
	for _, e := range outA["errors"].([]interface{}) {
		joinedA += e.(string) + " "
	}
	if !strings.Contains(joinedA, "example.com:443") {
		t.Fatalf("ошибка без упоминания заявленного хоста: %s", joinedA)
	}

	// allow + явный any_host: валидно, warning про заявленную сеть.
	// Раньше здесь был пайплайн вовсе без поля network, и он считался
	// валидным; теперь пустое поле равно deny, поэтому «разрешить» — это
	// явное network: allow плюс исполнимое объявление.
	raw2 := []byte(`format_version: "0.2"
pipeline:
  name: net_ok
  network: allow
  input:
    x: "hi"
  steps:
    - id: fx
      plugin: net-any-fixture
      bind:
        item: input.x
`)
	_, doc2 := postBytes(t, ts.URL+"/api/parse/pipeline", raw2)
	d2, _ := json.Marshal(doc2)
	code, out2 := postBytes(t, ts.URL+"/api/serialize/pipeline", d2)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out2)
	}
	if out2["ok"] != true {
		t.Fatalf("ok=%v errors=%v", out2["ok"], out2["errors"])
	}
	warns := ""
	for _, w := range out2["warnings"].([]interface{}) {
		warns += w.(string) + " "
	}
	if !strings.Contains(warns, "*:443") {
		t.Fatalf("нет warning про заявленную сеть *:443: %s", warns)
	}
}

func TestEditorPreservesGatePoliciesAndStructuredInput(t *testing.T) {
	ts, _ := gateTestServer(t)
	raw := []byte(`format_version: "0.2"
pipeline:
  name: policy_roundtrip
  gates: human_only
  input:
    values: [one, two]
    count: 3
    zero: 0
    disabled: false
    object:
      enabled: false
      count: 0
  steps:
    - id: review
      plugin: core/human_gate
      approval: human
      form:
        - field: input.count
          editable: true
          type: number
          format: text
      actions: [accept]
`)
	code, doc := postBytes(t, ts.URL+"/api/parse/pipeline", raw)
	if code != 200 {
		t.Fatalf("parse code=%d body=%v", code, doc)
	}
	if doc["gates"] != "human_only" {
		t.Fatalf("gates=%v", doc["gates"])
	}
	inputs := doc["input"].([]interface{})
	defaults := make(map[string]interface{}, len(inputs))
	for _, rawInput := range inputs {
		input := rawInput.(map[string]interface{})
		defaults[input["name"].(string)] = input["default"]
	}
	expected := map[string]interface{}{
		"values":   []interface{}{"one", "two"},
		"count":    float64(3),
		"zero":     float64(0),
		"disabled": false,
		"object": map[string]interface{}{
			"enabled": false,
			"count":   float64(0),
		},
	}
	if !reflect.DeepEqual(defaults, expected) {
		t.Fatalf("typed inputs=%v", defaults)
	}
	step := doc["steps"].([]interface{})[0].(map[string]interface{})
	if step["approval"] != "human" {
		t.Fatalf("approval=%v", step["approval"])
	}
	form := step["form"].([]interface{})[0].(map[string]interface{})
	if form["type"] != "number" || form["format"] != "text" {
		t.Fatalf("form metadata=%v", form)
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 || out["ok"] != true {
		t.Fatalf("serialize code=%d body=%v", code, out)
	}
	yamlText := out["yaml"].(string)
	if !strings.Contains(yamlText, "gates: human_only") || !strings.Contains(yamlText, "approval: human") || !strings.Contains(yamlText, "format: text") {
		t.Fatalf("metadata lost:\n%s", yamlText)
	}
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(yamlText))
	if err != nil {
		t.Fatal(err)
	}
	if pf.Pipeline.Gates != "human_only" || pf.Pipeline.Steps[0].Approval != "human" || pf.Pipeline.Steps[0].Form[0].Format != "text" {
		t.Fatalf("round-trip metadata=%+v", pf)
	}
	expectedCore := map[string]interface{}{
		"values":   []interface{}{"one", "two"},
		"count":    3,
		"zero":     0,
		"disabled": false,
		"object": map[string]interface{}{
			"enabled": false,
			"count":   0,
		},
	}
	if !reflect.DeepEqual(pf.Pipeline.Input, expectedCore) {
		t.Fatalf("typed inputs after round-trip=%v", pf.Pipeline.Input)
	}
}

func TestEditorRejectsInvalidGatePolicy(t *testing.T) {
	ts, _ := gateTestServer(t)
	doc := map[string]interface{}{
		"name": "bad_policy", "format_version": "0.2",
		"input": []interface{}{}, "steps": []interface{}{}, "gates": "typo", "unsupported": []interface{}{},
	}
	d, _ := json.Marshal(doc)
	code, out := postBytes(t, ts.URL+"/api/serialize/pipeline", d)
	if code != 200 || out["ok"] != false {
		t.Fatalf("invalid policy accepted: code=%d body=%v", code, out)
	}
}
