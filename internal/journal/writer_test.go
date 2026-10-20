package journal

// v0.23: журнал — надёжность (без мутаций, атомарный снапшот).

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/runctx"
)

// Event не мутирует переданный map (footgun для переиспользуемых мап).
func TestEventDoesNotMutateInput(t *testing.T) {
	j, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	kv := map[string]interface{}{"step": "x", "attempt": 1}
	j.Event("step_start", kv)
	if _, ok := kv["ts"]; ok {
		t.Fatal("Event мутировал входной map: появился ts")
	}
	if _, ok := kv["type"]; ok {
		t.Fatal("Event мутировал входной map: появился type")
	}
	// и в файле событие корректное
	raw, err := os.ReadFile(filepath.Join(j.Dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]interface{}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("битое событие: %v", err)
	}
	if ev["type"] != "step_start" || ev["step"] != "x" {
		t.Fatalf("событие некорректно: %v", ev)
	}
	if _, ok := ev["ts"]; !ok {
		t.Fatal("в файле нет ts")
	}
}

// Snapshot — context.json появляется атомарно, без .tmp-хвостов.
func TestNewJournalDoesNotTruncateExisting(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.Event("marker", map[string]interface{}{"value": "keep"})
	j.Close()
	if _, err := NewJournal(dir); err == nil {
		t.Fatal("повторное создание должно вернуть collision")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]interface{}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev["type"] != "marker" {
		t.Fatalf("старый журнал перезаписан: %v", ev)
	}
}

func TestSnapshotAtomic(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := runctx.NewCtx(map[string]interface{}{"input": "x"})
	ctx.SetStep("a", map[string]interface{}{"k": float64(1)})
	j.Snapshot(ctx)
	raw, err := os.ReadFile(filepath.Join(dir, "context.json"))
	if err != nil {
		t.Fatalf("context.json не появился: %v", err)
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("битый context.json: %v", err)
	}
	if data["steps"] == nil {
		t.Fatalf("steps нет в снапшоте: %v", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "context.json.tmp")); err == nil {
		t.Fatal("остался context.json.tmp — rename не сработал")
	}
}

func TestEventAndSnapshotReportSerializationErrors(t *testing.T) {
	j, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.Event("bad", map[string]interface{}{"value": math.NaN()}); err == nil {
		t.Fatal("Event должен вернуть ошибку сериализации")
	}
	if j.WriteErrors() != 1 {
		t.Fatalf("write errors=%d, want 1", j.WriteErrors())
	}
	ctx := runctx.NewCtx(map[string]interface{}{"bad": math.Inf(1)})
	if err := j.Snapshot(ctx); err == nil {
		t.Fatal("Snapshot должен вернуть ошибку сериализации")
	}
	if j.WriteErrors() != 2 {
		t.Fatalf("write errors=%d, want 2", j.WriteErrors())
	}
}

// P2 F-04: потеря снапшота — не «ничего страшного». Отказ по размеру обязан
// попасть в журнал (событие snapshot_lost) и в счётчик потерь: иначе ран
// рапортует об успехе, а --resume поднимается с устаревшим context.json.
func TestOversizedSnapshotIsRecordedInJournal(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	prev := maxSnapshotPayloadSize
	maxSnapshotPayloadSize = 512
	defer func() { maxSnapshotPayloadSize = prev }()

	ctx := runctx.NewCtx(map[string]interface{}{"payload": strings.Repeat("x", 4096)})
	err = j.Snapshot(ctx)
	if err == nil {
		t.Fatal("oversized snapshot обязан вернуть ошибку")
	}
	if j.SnapshotLosses() != 1 {
		t.Fatalf("snapshot losses=%d, want 1", j.SnapshotLosses())
	}
	if j.SnapshotLossError() == nil {
		t.Fatal("журнал обязан помнить причину первой потери")
	}
	// в ошибке — причина и потолок, но не данные контекста
	msg := err.Error()
	if !contains(msg, "exceeds") {
		t.Fatalf("в ошибке нет потолка: %v", err)
	}
	if contains(msg, "xxxx") {
		t.Fatalf("в ошибке снапшота утекли данные контекста: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "context.json")); statErr == nil {
		t.Fatal("context.json создан, хотя снапшот превысил потолок")
	}
	// потеря засчитана и в общем счётчике записей
	if j.WriteErrors() != 1 {
		t.Fatalf("write errors=%d, want 1", j.WriteErrors())
	}

	// повторная потеря того же отказа не засоряет append-only журнал:
	// событие одно, но потерь уже две.
	if err := j.Snapshot(ctx); err == nil {
		t.Fatal("повторный oversized snapshot обязан вернуть ошибку")
	}
	if j.SnapshotLosses() != 2 {
		t.Fatalf("snapshot losses=%d, want 2", j.SnapshotLosses())
	}
	j.Close()

	lost := eventsOfType(t, dir, "snapshot_lost")
	if len(lost) != 1 {
		t.Fatalf("событий snapshot_lost=%d, want 1: %v", len(lost), lost)
	}
	if reason, _ := lost[0]["reason"].(string); reason != "too_large" {
		t.Fatalf("reason=%q, want too_large: %v", lost[0]["reason"], lost[0])
	}
	if size, _ := lost[0]["bytes"].(float64); size <= 512 {
		t.Fatalf("событие не несёт размер снапшота: %v", lost[0])
	}
	if contains(string(mustJSON(t, lost[0])), "xxxx") {
		t.Fatalf("событие snapshot_lost утекает данные контекста: %v", lost[0])
	}
}

// P2 F-04: отказ записи (context.json.tmp занят каталогом) — тот же класс
// потерь, только не по размеру: состояние на диске устарело, ран обязан
// знать об этом.
func TestSnapshotWriteFailureIsRecordedInJournal(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	// каталог на месте context.json.tmp: os.WriteFile по этому пути падает
	// на любой ОС (EISDIR/Access denied), а не только на read-only каталоге.
	if err := os.Mkdir(filepath.Join(dir, "context.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := runctx.NewCtx(map[string]interface{}{"x": "y"})
	if err := j.Snapshot(ctx); err == nil {
		t.Fatal("Snapshot обязан вернуть ошибку записи")
	}
	if j.SnapshotLosses() != 1 {
		t.Fatalf("snapshot losses=%d, want 1", j.SnapshotLosses())
	}
	if _, statErr := os.Stat(filepath.Join(dir, "context.json")); statErr == nil {
		t.Fatal("context.json создан при провале записи")
	}
	j.Close()

	lost := eventsOfType(t, dir, "snapshot_lost")
	if len(lost) != 1 {
		t.Fatalf("событий snapshot_lost=%d, want 1: %v", len(lost), lost)
	}
	if reason, _ := lost[0]["reason"].(string); reason != "write" {
		t.Fatalf("reason=%q, want write: %v", lost[0]["reason"], lost[0])
	}
}

// contains — подстрока без импорта strings ради одного сравнения.
func contains(s, sub string) bool { return strings.Contains(s, sub) }

func eventsOfType(t *testing.T, dir, typ string) []map[string]interface{} {
	t.Helper()
	events, err := NewReader(dir).Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]interface{}
	for _, e := range events {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReaderAcceptsLargeEvent(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 100*1024)
	if err := j.Event("item_start", map[string]interface{}{"payload": payload}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	events, err := NewReader(dir).Events()
	if err != nil || len(events) != 1 {
		t.Fatalf("large event: count=%d err=%v", len(events), err)
	}
	if events[0]["payload"] != payload {
		t.Fatal("large event payload changed")
	}
}

func TestReaderRejectsMalformedEvent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte("{bad\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReader(dir).Events(); err == nil {
		t.Fatal("malformed event must be reported")
	}
}
