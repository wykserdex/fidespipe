package api

// v0.24: гейт из браузера — полный цикл через HTTP:
// POST /api/run (yes=false) → ран блокируется на gate_wait →
// POST /api/runs/<id>/gate → ран завершается.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/journal"
)

const gatePipeYAML = `format_version: "0.1"
pipeline:
  name: gate_api_demo
  input:
    note: "hello"
  steps:
    - id: review
      plugin: core/human_gate
      form:
        - { field: input.note, editable: true, type: string }
      actions: [accept, reject]
      on_reject: stop
`

// gateTestServer — GUI-сервер в tempdir: pipeline с гейтом, пустые plugins/runs.
func gateTestServer(t *testing.T) (*httptest.Server, string) {
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
	if err := os.WriteFile(filepath.Join(pipelines, "gate_demo.yaml"), []byte(gatePipeYAML), 0644); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(plugins, pipelines, runs)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, runs
}

func postJSON(t *testing.T, url string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]interface{}
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func getJSON(t *testing.T, url string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]interface{}
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

// waitRunStatus — поллинг статуса рана (20 c).
func waitRunStatus(t *testing.T, ts *httptest.Server, id, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, d := getJSON(t, ts.URL+"/api/runs/"+id)
		if d["status"] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("статус рана = %v, ждали %q (20 c)", d["status"], want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitPendingGate — поллинг GET /api/runs/<id>/gate: pending=true.
func waitPendingGate(t *testing.T, ts *httptest.Server, id string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, d := getJSON(t, ts.URL+"/api/runs/"+id+"/gate")
		if code == 200 && d["pending"] == true {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("гейт не вошёл в pending за 20 c (code=%d, body=%v)", code, d)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func runJournalEvents(t *testing.T, runsDir, id string) []map[string]interface{} {
	t.Helper()
	events, err := journal.NewReader(filepath.Join(runsDir, id)).Events()
	if err != nil {
		t.Fatalf("журнал: %v", err)
	}
	return events
}

func TestAPIThroughGate(t *testing.T) {
	ts, runs := gateTestServer(t)

	// 1) запуск без --yes: ран доходит до гейта и ждёт
	code, start := postJSON(t, ts.URL+"/api/run", map[string]interface{}{"file": "gate_demo.yaml", "yes": false})
	if code != 202 {
		t.Fatalf("POST /api/run: code=%d body=%v (want 202)", code, start)
	}
	runID, _ := start["run"].(string)
	if runID == "" || !strings.Contains(runID, "gate_api_demo") {
		t.Fatalf("run = %v (want id с именем пайплайна)", start["run"])
	}

	// 2) гейт в pending, с формой и действиями из манифеста
	gw := waitPendingGate(t, ts, runID)
	if gw["step"] != "review" {
		t.Fatalf("step = %v", gw["step"])
	}
	actions, _ := gw["actions"].([]interface{})
	if len(actions) != 2 {
		t.Fatalf("actions = %v", gw["actions"])
	}

	// 3) решение из «браузера»: accept + правка input.note
	code, _ = postJSON(t, ts.URL+"/api/runs/"+runID+"/gate", map[string]interface{}{
		"action": "accept",
		"edits":  map[string]interface{}{"input.note": "из браузера"},
	})
	if code != 202 {
		t.Fatalf("POST gate: code=%d (want 202)", code)
	}

	// 4) ран завершается ок
	waitRunStatus(t, ts, runID, "ok")

	// 5) журнал: gate_wait → gate_decision accept с материализованной правкой
	events := runJournalEvents(t, runs, runID)
	var sawWait, sawAccept bool
	var lastDec map[string]interface{}
	for _, e := range events {
		switch e["type"] {
		case "gate_wait":
			sawWait = true
		case "gate_decision":
			lastDec = e
			if e["action"] == "accept" {
				sawAccept = true
			}
		}
	}
	if !sawWait || !sawAccept {
		t.Fatalf("журнал: gate_wait=%v gate_decision_accept=%v", sawWait, sawAccept)
	}
	mat, _ := lastDec["materialized"].(map[string]interface{})
	if mat == nil || mat["note"] != "из браузера" {
		t.Fatalf("materialized = %v (want note=«из браузера»)", lastDec["materialized"])
	}

	// 6) повторный submit после завершения — 409
	code, body := postJSON(t, ts.URL+"/api/runs/"+runID+"/gate", map[string]interface{}{"action": "reject"})
	if code != 409 {
		t.Fatalf("повторный POST gate: code=%d body=%v (want 409)", code, body)
	}
}

func TestAPIThroughGateRejectAborts(t *testing.T) {
	ts, _ := gateTestServer(t)
	_, start := postJSON(t, ts.URL+"/api/run", map[string]interface{}{"file": "gate_demo.yaml", "yes": false})
	runID, _ := start["run"].(string)
	waitPendingGate(t, ts, runID)
	code, _ := postJSON(t, ts.URL+"/api/runs/"+runID+"/gate", map[string]interface{}{"action": "reject"})
	if code != 202 {
		t.Fatalf("POST gate: code=%d", code)
	}
	// on_reject: stop → ран завершён как aborted
	waitRunStatus(t, ts, runID, "aborted")
}

func TestAPIRunYesHasNoPendingGate(t *testing.T) {
	ts, runs := gateTestServer(t)
	_, start := postJSON(t, ts.URL+"/api/run", map[string]interface{}{"file": "gate_demo.yaml", "yes": true})
	runID, _ := start["run"].(string)
	waitRunStatus(t, ts, runID, "ok")
	// гейт авто-принят (--yes): pending нет, submit — 409
	code, d := getJSON(t, ts.URL+"/api/runs/"+runID+"/gate")
	if code != 200 || d["pending"] != false {
		t.Fatalf("GET gate: code=%d body=%v (want pending=false)", code, d)
	}
	code, _ = postJSON(t, ts.URL+"/api/runs/"+runID+"/gate", map[string]interface{}{"action": "accept"})
	if code != 409 {
		t.Fatalf("POST gate: code=%d (want 409)", code)
	}
	// журнал: gate_decision с auto=true (как и раньше)
	found := false
	for _, e := range runJournalEvents(t, runs, runID) {
		if e["type"] == "gate_decision" && e["auto"] == true {
			found = true
		}
	}
	if !found {
		t.Fatal("gate_decision auto=true не найден в журнале --yes рана")
	}
}

func TestAPIRunConcurrentBusy(t *testing.T) {
	ts, _ := gateTestServer(t)
	_, start := postJSON(t, ts.URL+"/api/run", map[string]interface{}{"file": "gate_demo.yaml", "yes": false})
	runID, _ := start["run"].(string)
	waitPendingGate(t, ts, runID)
	// второй ран, пока первый на гейте — 409
	code, _ := postJSON(t, ts.URL+"/api/run", map[string]interface{}{"file": "gate_demo.yaml", "yes": false})
	if code != 409 {
		t.Fatalf("второй POST /api/run: code=%d (want 409)", code)
	}
	// разблокируем первый, чтобы тест не оставил висеть lock до cleanup
	postJSON(t, ts.URL+"/api/runs/"+runID+"/gate", map[string]interface{}{"action": "accept"})
	waitRunStatus(t, ts, runID, "ok")
}
