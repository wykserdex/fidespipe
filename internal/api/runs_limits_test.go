package api

// P2 F-03: потолки ответа /api/runs и /api/runs/<id> — oversized-журнал
// обрезается по событиям, обрезание помечается truncated, курсор since
// (live-поллинг) остаётся точным, а summary считается по всему журналу.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/journal"
)

// oversizedJournal — n событий step_end, обёрнутые в run_start/run_end.
func oversizedJournal(t *testing.T, runsDir, id string, n int) {
	t.Helper()
	dir := filepath.Join(runsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := journal.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.Event("run_start", map[string]interface{}{"pipeline": "demo"})
	for i := 0; i < n; i++ {
		j.Event("step_end", map[string]interface{}{"step": "s1", "n": i})
	}
	j.Event("run_end", map[string]interface{}{"ok": n, "aborted": 0})
	j.Close()
}

func runsTestServer(t *testing.T, runsDir string) *Server {
	t.Helper()
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	pipelines := filepath.Join(dir, "pipelines")
	for _, d := range []string{plugins, pipelines, runsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return NewServer(plugins, pipelines, runsDir)
}

// Журнал больше потолка ответа: события обрезаются хвостом, total точный,
// статус/пайплайн считаются по всему журналу.
func TestRunDetailTruncatesOversizedJournal(t *testing.T) {
	runs := t.TempDir()
	srv := runsTestServer(t, runs)
	ts := newTestServer(t, srv)
	oversizedJournal(t, runs, "big-run", maxRunResponseEvents+1)

	code, body := getJSON(t, ts.URL+"/api/runs/big-run")
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	events, _ := body["events"].([]interface{})
	if len(events) != maxRunResponseEvents {
		t.Fatalf("в ответе %d событий, want %d", len(events), maxRunResponseEvents)
	}
	if body["truncated"] != true {
		t.Fatalf("обрезанный журнал без truncated: %v", body)
	}
	// журнал: run_start + (limit+1) step_end + run_end
	if body["total"] != float64(maxRunResponseEvents+3) {
		t.Fatalf("total=%v, want %d", body["total"], maxRunResponseEvents+3)
	}
	if body["first"] != float64(3) {
		t.Fatalf("first=%v, want 3 (окно уехало в хвост)", body["first"])
	}
	if body["status"] != "ok" || body["pipeline"] != "demo" {
		t.Fatalf("summary по обрезанному ответу: status=%v pipeline=%v", body["status"], body["pipeline"])
	}
	// последнее событие журнала (run_end) в окне — хвост, а не начало
	last, _ := events[len(events)-1].(map[string]interface{})
	if last["type"] != "run_end" {
		t.Fatalf("хвост журнала не в ответе: %v", last)
	}
	// старые поля ответа на месте
	if body["id"] != "big-run" {
		t.Fatalf("id=%v", body["id"])
	}
	if _, ok := body["context"]; !ok {
		t.Fatalf("нет поля context: %v", body)
	}
}

// Журнал в потолке: truncated=false, total == числу событий, курсор next == total.
func TestRunDetailSmallJournalIsNotTruncated(t *testing.T) {
	runs := t.TempDir()
	srv := runsTestServer(t, runs)
	ts := newTestServer(t, srv)
	oversizedJournal(t, runs, "small-run", 5)

	code, body := getJSON(t, ts.URL+"/api/runs/small-run")
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	events, _ := body["events"].([]interface{})
	if body["truncated"] != false {
		t.Fatalf("truncated=%v, want false", body["truncated"])
	}
	if len(events) != 7 || body["total"] != float64(7) || body["first"] != float64(0) {
		t.Fatalf("окно не покрывает журнал: events=%d body=%v", len(events), body)
	}
}

// since-поллинг не теряет события: total совпадает с next, пока не покрыт
// весь журнал; после полного покрытия next == total и событий нет.
func TestRunJournalSincePollingKeepsCursor(t *testing.T) {
	runs := t.TempDir()
	srv := runsTestServer(t, runs)
	ts := newTestServer(t, srv)
	oversizedJournal(t, runs, "poll-run", 4)

	code, body := getJSON(t, ts.URL+"/api/runs/poll-run/journal?since=2")
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, body)
	}
	events, _ := body["events"].([]interface{})
	if len(events) != 4 || body["total"] != float64(6) {
		t.Fatalf("since=2: events=%d body=%v", len(events), body)
	}
	if body["first"] != float64(2) || body["next"] != float64(6) || body["truncated"] != false {
		t.Fatalf("курсор since=2 разъехался: %v", body)
	}
	first, _ := events[0].(map[string]interface{})
	if first["type"] != "step_end" {
		t.Fatalf("since=2 отдал не то событие: %v", first)
	}
	code, tail := getJSON(t, ts.URL+"/api/runs/poll-run/journal?since=6")
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, tail)
	}
	if rest, _ := tail["events"].([]interface{}); len(rest) != 0 {
		t.Fatalf("since=6 вернул события: %v", rest)
	}
	if tail["next"] != float64(6) || tail["total"] != float64(6) {
		t.Fatalf("курсор убежал за конец: %v", tail)
	}
}

// Список ранов: summary считается по ВСЕМ событиям (потоковый проход), даже
// когда журнал больше потолка ответа деталки.
func TestRunListSummaryCountsWholeJournal(t *testing.T) {
	runs := t.TempDir()
	srv := runsTestServer(t, runs)
	ts := newTestServer(t, srv)
	oversizedJournal(t, runs, "list-run", maxRunResponseEvents+1)

	resp, err := http.Get(ts.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("ранов в списке %d, want 1", len(list))
	}
	m := list[0]
	if m["id"] != "list-run" || m["status"] != "ok" || m["pipeline"] != "demo" {
		t.Fatalf("summary: %v", m)
	}
	// run_start + (limit+1) step_end + run_end
	if m["events"] != float64(maxRunResponseEvents+3) {
		t.Fatalf("summary.events=%v, want %d", m["events"], maxRunResponseEvents+3)
	}
	if m["steps"] != float64(maxRunResponseEvents+1) {
		t.Fatalf("summary.steps=%v", m["steps"])
	}
	if m["truncated"] != false {
		t.Fatalf("summary обрезан по потолку ScanMeta: %v", m)
	}
	if m["started"] == "" || m["last"] == "" {
		t.Fatalf("summary без времени: %v", m)
	}
}

// Стабильный потолок списка: самые новые maxRunsListed ранов, факт обрезания —
// в заголовке (форма ответа — массив — не меняется).
func TestRunListCapsNumberOfRuns(t *testing.T) {
	runs := t.TempDir()
	srv := runsTestServer(t, runs)
	ts := newTestServer(t, srv)
	total := maxRunsListed + 5
	for i := 0; i < total; i++ {
		oversizedJournal(t, runs, fmt.Sprintf("run-%04d", i), 1)
	}

	resp, err := http.Get(ts.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != maxRunsListed {
		t.Fatalf("ранов в ответе %d, want %d", len(list), maxRunsListed)
	}
	if resp.Header.Get("X-Fidespipe-Runs-Truncated") != "true" {
		t.Fatalf("нет заголовка обрезания: %v", resp.Header)
	}
	if list[0]["id"] != fmt.Sprintf("run-%04d", total-1) {
		t.Fatalf("список не отсортирован по свежести: %v", list[0])
	}
}

// Summary по потоку и по срезу событий совпадают (один код расчёта).
func TestSummarizeRunStreamMatchesSliceSummary(t *testing.T) {
	runs := t.TempDir()
	dir := filepath.Join(runs, "same-run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := journal.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.Event("run_start", map[string]interface{}{"pipeline": "demo"})
	j.Event("step_end", map[string]interface{}{"step": "a"})
	j.Event("step_skipped", map[string]interface{}{"step": "b"})
	j.Event("run_end", map[string]interface{}{"aborted": float64(2)})
	j.Close()
	events, err := journal.NewReader(dir).Events()
	if err != nil {
		t.Fatal(err)
	}
	sliceSummary := summarizeRun(dir, events)
	streamSummary := runSummary(dir)
	for _, key := range []string{"status", "pipeline", "steps", "events", "started", "last", "id", "dir"} {
		if fmt.Sprint(sliceSummary[key]) != fmt.Sprint(streamSummary[key]) {
			t.Fatalf("%s: по срезу %v, по потоку %v", key, sliceSummary[key], streamSummary[key])
		}
	}
	if streamSummary["status"] != "aborted" {
		t.Fatalf("статус: %v", streamSummary["status"])
	}
	if streamSummary["truncated"] != false {
		t.Fatalf("truncated=%v", streamSummary["truncated"])
	}
}

// Битый журнал: summary остаётся ответом (с truncated), а не 500/пустотой.
func TestRunSummaryMarksCorruptJournalAsTruncated(t *testing.T) {
	dir := t.TempDir()
	journalPath := filepath.Join(dir, "journal.jsonl")
	if err := os.WriteFile(journalPath, []byte("{\"type\":\"run_start\",\"pipeline\":\"demo\"}\n{oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := runSummary(dir)
	if summary["truncated"] != true {
		t.Fatalf("битый журнал без truncated: %v", summary)
	}
	if summary["pipeline"] != "demo" {
		t.Fatalf("разобранная часть потеряна: %v", summary)
	}
	if _, err := journal.NewReader(dir).Events(); err == nil || !strings.Contains(err.Error(), "строка 2") {
		t.Fatalf("Events() по битому журналу: err=%v", err)
	}
}
