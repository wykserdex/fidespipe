package execution

// P2 F-04: потерянный снапшот контекста обязана быть видна в терминальном
// статусе рана. Раньше j.Snapshot(ctx) в горячих путях возвращал ошибку в
// пустоту, а journalWriteError проверялся только после run_end — ран
// рапортовал «ок» и exit code 0 при устаревшем context.json на диске.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// blockSnapshots — кладёт каталог на место context.json.tmp в каталоге рана:
// os.WriteFile по этому пути падает на любой ОС, и каждый снимок контекста
// теряется. Каталог создаётся ДО рана: store.Create открывает journal.jsonl
// с O_EXCL, а самого каталога рана для этого достаточно.
func blockSnapshots(t *testing.T, runsDir, runID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(runsDir, runID, "context.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// appendJournalEvent — дописывает в конец журнала ровно то, что пишет сам
// Journal (append-only: переписывать прошлые события нельзя). Нужно, чтобы
// разложить состояние «потеря снапшота произошла в ПРОШЛОМ прогоне» — её
// счётчик у нового Journal-объекта пуст, а состояние на диске то же.
func appendJournalEvent(t *testing.T, dir string, kv map[string]interface{}) {
	t.Helper()
	kv["ts"] = time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(kv)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

func terminalEvent(t *testing.T, events []map[string]interface{}) map[string]interface{} {
	t.Helper()
	var last map[string]interface{}
	for _, e := range events {
		switch e["type"] {
		case "run_end", "run_failed", "run_cancelled":
			last = e
		}
	}
	if last == nil {
		t.Fatal("в журнале нет терминального события рана")
	}
	return last
}

// foreachGatePipeline — батч из одного элемента с гейтом: resume такого рана
// идёт по ветке «все элементы уже пройдены», где ранний runner писал run_end,
// ни разу не снимая снапшот.
func foreachGatePipeline() *pipeline.PipelineFile {
	return &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:    "resume_snapshot",
			Input:   map[string]interface{}{"items": []interface{}{"only"}},
			Foreach: "input.items",
			Steps: []pipeline.Step{{
				ID:     "review",
				Plugin: "core/human_gate",
				Form:   []pipeline.FormField{{Field: "input.item", Type: "string"}},
			}},
		},
	}
}

// Потерянный снапшот = терминальный отказ: run_end был бы враньём (на диске
// лежит устаревший context.json), поэтому в журнале run_failed с кодом
// snapshot_lost, а вызывающий получает ошибку с тем же кодом.
func TestLostSnapshotFailsRunWithDedicatedCode(t *testing.T) {
	dir := t.TempDir()
	pf := resumeGatePipeline()
	blockSnapshots(t, dir, "lost-snapshot")

	stats, err := Run(pf, permissiveEngine{}, RunOptions{Yes: true, Quiet: true, RunID: "lost-snapshot", RunsDir: dir})
	if err == nil {
		t.Fatalf("ран с потерянным снапшотом обязан упасть, ok=%v", stats)
	}
	if code := ErrorCode(err); code != "snapshot_lost" {
		t.Fatalf("код ошибки %q, ожидался snapshot_lost (%v)", code, err)
	}
	// в ошибке — причина и счётчик, но не данные контекста
	if !contains(err.Error(), "снапшот") {
		t.Fatalf("в ошибке нет упоминания снапшота: %v", err)
	}

	events := runEvents(t, stats.RunDir)
	if n := countRunEvents(events, "snapshot_lost"); n != 1 {
		t.Fatalf("событий snapshot_lost=%d, want 1", n)
	}
	term := terminalEvent(t, events)
	if term["type"] != "run_failed" {
		t.Fatalf("терминальное событие %v, ожидался run_failed", term["type"])
	}
	if code, _ := term["code"].(string); code != "snapshot_lost" {
		t.Fatalf("run_failed без code=snapshot_lost: %v", term)
	}
	if n, _ := term["snapshot_losses"].(float64); n < 1 {
		t.Fatalf("run_failed не несёт число потерянных снапшотов: %v", term)
	}
	if _, err := os.Stat(filepath.Join(stats.RunDir, "context.json")); err == nil {
		t.Fatal("context.json не должен существовать при провале записи снапшота")
	}
}

// Потеря прошлого прогона наследуется resume: журнал общий, состояние на
// диске то же, а курсор resume берётся из журнала. Продолжать можно, но
// терминальный вердикт «ok» был бы ложью — disk-состояние отстаёт от курсора.
func TestResumeInheritsPriorSnapshotLoss(t *testing.T) {
	dir := t.TempDir()
	pf := foreachGatePipeline()
	stats, err := Run(pf, permissiveEngine{}, RunOptions{Yes: true, Quiet: true, RunID: "resume-lost", RunsDir: dir})
	if err != nil {
		t.Fatalf("эталонный прогон: %v", err)
	}
	appendJournalEvent(t, stats.RunDir, map[string]interface{}{"type": "snapshot_lost", "reason": "write", "bytes": 4096})

	// pipeline_hash сверяется по содержимому YAML, поэтому resume берёт
	// свежий файл — как это делает CLI, перечитывая YAML.
	resumed, err := Run(foreachGatePipeline(), permissiveEngine{}, RunOptions{Yes: true, Quiet: true, Resume: "resume-lost", RunsDir: dir})
	if err == nil {
		t.Fatalf("resume после потери снапшота обязан сообщить об этом, ok=%v", resumed)
	}
	if code := ErrorCode(err); code != "snapshot_lost" {
		t.Fatalf("код ошибки %q, ожидался snapshot_lost (%v)", code, err)
	}
	if resumed.SnapshotLosses != 1 {
		t.Fatalf("stats.SnapshotLosses=%d, want 1", resumed.SnapshotLosses)
	}
	events := runEvents(t, resumed.RunDir)
	term := terminalEvent(t, events)
	if term["type"] != "run_failed" {
		t.Fatalf("терминальное событие %v, ожидался run_failed", term["type"])
	}
	if n, _ := term["snapshot_losses"].(float64); n != 1 {
		t.Fatalf("run_failed не наследовал потерю прошлого прогона: %v", term)
	}
}

// Обратная сторона: обычный ран не должен ни попасть под snapshot_lost, ни
// потерять run_end (иначе «страховка» от потери снапшота тихо ломает зелёные
// раны и их коды выхода).
func TestHealthyRunKeepsRunEnd(t *testing.T) {
	dir := t.TempDir()
	pf := resumeGatePipeline()

	stats, err := Run(pf, permissiveEngine{}, RunOptions{Yes: true, Quiet: true, RunID: "healthy", RunsDir: dir})
	if err != nil {
		t.Fatalf("обычный ран обязан завершиться без ошибки: %v", err)
	}
	if stats.SnapshotLosses != 0 {
		t.Fatalf("здоровый ран: SnapshotLosses=%d", stats.SnapshotLosses)
	}
	events := runEvents(t, stats.RunDir)
	if n := countRunEvents(events, "snapshot_lost"); n != 0 {
		t.Fatalf("событий snapshot_lost=%d в здоровом ране", n)
	}
	term := terminalEvent(t, events)
	if term["type"] != "run_end" {
		t.Fatalf("терминальное событие %v, ожидался run_end", term["type"])
	}
	if _, err := os.Stat(filepath.Join(stats.RunDir, "context.json")); err != nil {
		t.Fatalf("context.json не записан: %v", err)
	}
}
