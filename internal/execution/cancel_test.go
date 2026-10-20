package execution

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/runctx"
)

func requirePython(t *testing.T) {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if _, err := exec.LookPath(name); err == nil {
			return
		}
	}
	t.Skip("python не найден — пропускаю")
}

func runEvents(t *testing.T, dir string) []map[string]interface{} {
	t.Helper()
	events, err := journal.NewReader(dir).Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func countRunEvents(events []map[string]interface{}, typ string) int {
	n := 0
	for _, e := range events {
		if e["type"] == typ {
			n++
		}
	}
	return n
}

// writeScriptPlugin — временный плагин с одним входом item и выходом value.
func writeScriptPlugin(t *testing.T, dir, id, script string) string {
	t.Helper()
	d := filepath.Join(dir, id)
	if err := os.MkdirAll(d, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"id": id, "version": "0.1", "platform_api": "0.1",
		"runtime": map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":   map[string]interface{}{"item": map[string]interface{}{"type": "string", "from": "input.item"}},
		"output":  map[string]interface{}{"value": map[string]interface{}{"type": "string"}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "plugin.yaml"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "main.py"), []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	return d
}

func writeSleeper(t *testing.T, dir string) string {
	t.Helper()
	d := filepath.Join(dir, "sleeper")
	if err := os.MkdirAll(d, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"id": "sleeper", "version": "0.1", "platform_api": "0.1",
		"runtime": map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":   map[string]interface{}{},
		"output":  map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(d, "plugin.yaml"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	// спит 30с без дочерних (на Unix группа убивается вместе с внуком —
	// см. TestExecTimeoutKillsProcessGroup; на Windows Job Object нет,
	// поэтому cancel-тест без внука, иначе Wait висит на унаследованном пайпе).
	py := "import sys,json,time\njson.load(sys.stdin)\ntime.sleep(30)\njson.dump({'status':'ok','output':{'done':True}},sys.stdout)\n"
	if err := os.WriteFile(filepath.Join(d, "main.py"), []byte(py), 0644); err != nil {
		t.Fatal(err)
	}
	return d
}

// mapEngine — движок тестов: манифест читается из каталога плагина на диске.
type mapEngine struct{}

type permissiveEngine struct{}

func (permissiveEngine) LoadManifest(ref string) (*pipeline.Manifest, error) {
	return &pipeline.Manifest{ID: ref}, nil
}

func (m *mapEngine) LoadManifest(ref string) (*pipeline.Manifest, error) {
	// ref — абсолютный путь к директории
	raw, err := os.ReadFile(filepath.Join(ref, "plugin.yaml"))
	if err != nil {
		return nil, err
	}
	var manifest pipeline.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	manifest.Dir = ref
	return &manifest, nil
}

// TestCancelSleeper — отмена через 200ms: группа умирает <5с, статус cancelled, не timeout.
func TestCancelSleeper(t *testing.T) {
	dir := t.TempDir()
	sleeperDir := writeSleeper(t, filepath.Join(dir, "plugins"))
	runsDir := filepath.Join(dir, "runs")
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "cancel_demo",
			Input: map[string]interface{}{},
			Steps: []pipeline.Step{{ID: "sleep", Plugin: sleeperDir, Timeout: pipeline.Duration{}}},
		},
	}
	eng := &mapEngine{}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var stats RunStats
	start := time.Now()
	go func() {
		var err error
		stats, err = Run(pf, eng, RunOptions{Quiet: true, RunsDir: runsDir, Ctx: runCtx,
			Trusted: trustPluginsDir(t, filepath.Join(dir, "plugins"))})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		dur := time.Since(start)
		if err == nil {
			t.Fatalf("ожидалась отмена, ран завершился ok=%v", stats)
		}
		if dur > 10*time.Second {
			t.Fatalf("отмена заняла %s (want <10s)", dur)
		}
		msg := err.Error()
		if !(contains(msg, "cancelled") || contains(msg, "отмен")) {
			t.Fatalf("статус не cancelled: %v", err)
		}
		// журнал: run_cancelled, затем snapshot; --resume должен поднимать
		rd := journal.NewReader(stats.RunDir)
		events, _ := rd.Events()
		sawCancel := false
		for _, e := range events {
			if e["type"] == "run_cancelled" {
				sawCancel = true
				if code, _ := e["code"].(string); code != "cancelled" {
					t.Fatalf("run_cancelled без code=cancelled: %v", e)
				}
			}
			if e["type"] == "run_failed" {
				if code, _ := e["code"].(string); code == "timeout" {
					t.Fatalf("отмена превратилась в timeout")
				}
			}
		}
		if !sawCancel {
			types := []string{}
			for _, e := range events {
				if ty, _ := e["type"].(string); ty != "" {
					types = append(types, ty)
				}
			}
			t.Fatalf("нет run_cancelled, события: %v", types)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ран не завершился за 30с после cancel")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

func TestResumeRejectsDifferentPipeline(t *testing.T) {
	dir := t.TempDir()
	base := &pipeline.PipelineFile{FormatVersion: "0.2", Pipeline: pipeline.Pipeline{Name: "hash", Input: map[string]interface{}{}}}
	if _, err := Run(base, permissiveEngine{}, RunOptions{Quiet: true, RunID: "hash-run", RunsDir: dir}); err != nil {
		t.Fatal(err)
	}
	changed := &pipeline.PipelineFile{FormatVersion: "0.2", Pipeline: pipeline.Pipeline{Name: "other", Input: map[string]interface{}{}}}
	_, err := Run(changed, permissiveEngine{}, RunOptions{Quiet: true, Resume: "hash-run", RunsDir: dir})
	if err == nil || !contains(err.Error(), "pipeline identity mismatch") {
		t.Fatalf("expected identity mismatch, got %v", err)
	}
}

func TestRetryDelayIsBounded(t *testing.T) {
	st := &pipeline.Step{Retry: &pipeline.Retry{Delay: pipeline.Duration{Duration: time.Hour}, Backoff: "exponential"}}
	if got := retryDelay(st, 10); got != pipeline.MaxRetryDelay {
		t.Fatalf("retry delay=%s, want cap %s", got, pipeline.MaxRetryDelay)
	}
}

func TestRunRejectsReservedBuiltin(t *testing.T) {
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "reserved",
			Input: map[string]interface{}{},
			Steps: []pipeline.Step{{ID: "s", Plugin: "core/does_not_exist"}},
		},
	}
	_, err := Run(pf, permissiveEngine{}, RunOptions{Yes: true, Quiet: true, RunsDir: t.TempDir()})
	if err == nil || !contains(err.Error(), "неизвестный встроенный модуль") {
		t.Fatalf("expected reserved builtin error, got %v", err)
	}
}

func TestRunRejectsInvalidNetworkPolicy(t *testing.T) {
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:    "network",
			Network: "denny",
			Input:   map[string]interface{}{},
		},
	}
	_, err := Run(pf, permissiveEngine{}, RunOptions{Quiet: true, RunsDir: t.TempDir()})
	if err == nil || !contains(err.Error(), "network") {
		t.Fatalf("expected invalid network policy error, got %v", err)
	}
}

func TestRunRejectsInvalidApprovalPolicy(t *testing.T) {
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "approval",
			Input: map[string]interface{}{},
			Steps: []pipeline.Step{{ID: "gate", Plugin: "core/human_gate", Approval: "humn"}},
		},
	}
	_, err := Run(pf, permissiveEngine{}, RunOptions{Yes: true, Quiet: true, RunsDir: t.TempDir()})
	if err == nil || !contains(err.Error(), "approval") {
		t.Fatalf("expected invalid approval policy error, got %v", err)
	}
}

func TestCloneCtxRejectsNaN(t *testing.T) {
	ctx := &runctx.Ctx{Data: map[string]interface{}{"value": math.NaN()}}
	if _, err := cloneCtx(ctx); err == nil {
		t.Fatal("cloneCtx accepted NaN")
	}
}

func TestRunParallelRejectsNaNContext(t *testing.T) {
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "nan_context",
			Input: map[string]interface{}{"value": math.NaN()},
			Steps: []pipeline.Step{
				{ID: "a", Plugin: "fake", ParallelGroup: "g"},
				{ID: "b", Plugin: "fake", ParallelGroup: "g"},
			},
		},
	}
	_, err := Run(pf, permissiveEngine{}, RunOptions{Quiet: true, RunsDir: t.TempDir()})
	if err == nil || !contains(err.Error(), "serialization") {
		t.Fatalf("expected context serialization error, got %v", err)
	}
}

func TestResumeCursorDoesNotLoopOnHugeIndex(t *testing.T) {
	dir := t.TempDir()
	j, err := journal.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Event("item_end", map[string]interface{}{"item_index": 1 << 30, "status": "ok"}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	result := make(chan struct {
		idx   int
		stats RunStats
		err   error
	}, 1)
	go func() {
		idx, stats, err := resumeCursor(dir)
		result <- struct {
			idx   int
			stats RunStats
			err   error
		}{idx, stats, err}
	}()
	select {
	case got := <-result:
		if got.err != nil || got.idx != 0 {
			t.Fatalf("resumeCursor: idx=%d stats=%+v err=%v", got.idx, got.stats, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resumeCursor did not stop on a sparse huge index")
	}
}

// ── retry-политика и коды платформенных ошибок (PROTOCOL §3/§6) ─────────

// exit>=2 + retryable:true — платформенная ошибка: ран стопится на первой
// попытке, несмотря на on_error: retry с retry.attempts: 3.
func TestRunPlatformErrorWithRetryableFlagIsNotRetried(t *testing.T) {
	requirePython(t)
	dir := t.TempDir()
	script := "import json,sys\njson.load(sys.stdin)\n" +
		"print(json.dumps({'status':'error','error':{'code':'bad_input','message':'boom','retryable':True}}))\n" +
		"sys.exit(2)\n"
	pluginDir := writeScriptPlugin(t, filepath.Join(dir, "plugins"), "boom2", script)
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "platform_no_retry",
			Input: map[string]interface{}{"item": "x"},
			Steps: []pipeline.Step{{
				ID: "boom", Plugin: pluginDir, OnError: "retry",
				Timeout: pipeline.Duration{Duration: 15 * time.Second},
				Retry:   &pipeline.Retry{Attempts: 3, Delay: pipeline.Duration{Duration: time.Millisecond}},
			}},
		},
	}
	_, err := Run(pf, &mapEngine{}, RunOptions{Quiet: true, RunsDir: filepath.Join(dir, "runs"),
		Trusted: trustPluginsDir(t, filepath.Join(dir, "plugins"))})
	if err == nil {
		t.Fatal("платформенная ошибка обязана остановить ран")
	}
	if code := ErrorCode(err); code != "platform:bad_input" {
		t.Fatalf("код рановой ошибки = %q, want platform:bad_input (один префикс, без platform:platform:)", code)
	}
	stats, statsErr := latestRunStats(t, filepath.Join(dir, "runs"))
	if statsErr != nil {
		t.Fatal(statsErr)
	}
	if n := countRunEvents(runEvents(t, stats.RunDir), "step_start"); n != 1 {
		t.Fatalf("exit>=2 не ретраится (PROTOCOL §3), попыток в журнале: %d", n)
	}
}

// exit>=2 без конверта → crash, но префикс `platform:` всё равно один.
func TestRunPlatformErrorCodeHasSinglePrefix(t *testing.T) {
	requirePython(t)
	dir := t.TempDir()
	script := "import json,sys\njson.load(sys.stdin)\nsys.exit(2)\n"
	pluginDir := writeScriptPlugin(t, filepath.Join(dir, "plugins"), "bare_crash", script)
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "platform_crash",
			Input: map[string]interface{}{"item": "x"},
			Steps: []pipeline.Step{{
				ID: "crash", Plugin: pluginDir, OnError: "skip",
				Timeout: pipeline.Duration{Duration: 15 * time.Second},
			}},
		},
	}
	_, err := Run(pf, &mapEngine{}, RunOptions{Quiet: true, RunsDir: filepath.Join(dir, "runs"),
		Trusted: trustPluginsDir(t, filepath.Join(dir, "plugins"))})
	if err == nil {
		t.Fatal("платформенная ошибка обязана остановить ран (on_error=skip не спасает)")
	}
	if code := ErrorCode(err); code != "platform:crash" {
		t.Fatalf("код рановой ошибки = %q, want platform:crash", code)
	}
}

// ── батч: частичные aborts не роняют ран (PROTOCOL §6, scope stop) ─────

// foreach: доменный stop глушит только текущий элемент; ран доходит до конца
// и возвращает nil — на этом и строит exit-код CLI (0 для завершённого батча).
func TestRunBatchSurvivesPartialAborts(t *testing.T) {
	requirePython(t)
	dir := t.TempDir()
	script := "import json,sys\nv=json.load(sys.stdin).get('item','')\n" +
		"if v=='bad':\n" +
		"    print(json.dumps({'status':'error','error':{'code':'bad_value','message':'nope','retryable':False}}))\n" +
		"    sys.exit(1)\n" +
		"print(json.dumps({'status':'ok','output':{'value':v}}))\n"
	pluginDir := writeScriptPlugin(t, filepath.Join(dir, "plugins"), "itemcheck", script)
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:        "batch_partial",
			Input:       map[string]interface{}{"values": []interface{}{"ok1", "bad", "ok2"}},
			Foreach:     "input.values",
			ForeachItem: "item",
			Steps: []pipeline.Step{{
				ID: "check", Plugin: pluginDir, OnError: "stop",
				Timeout: pipeline.Duration{Duration: 15 * time.Second},
			}},
		},
	}
	stats, err := Run(pf, &mapEngine{}, RunOptions{Quiet: true, RunsDir: filepath.Join(dir, "runs"),
		Trusted: trustPluginsDir(t, filepath.Join(dir, "plugins"))})
	if err != nil {
		t.Fatalf("частичный abort в батче не должен валить ран: %v", err)
	}
	if stats.OK != 2 || stats.Aborted != 1 {
		t.Fatalf("ожидалось ok=2 aborted=1, got %+v", stats)
	}
	events := runEvents(t, stats.RunDir)
	if n := countRunEvents(events, "item_aborted"); n != 1 {
		t.Fatalf("ожидался один item_aborted, got %d", n)
	}
	if countRunEvents(events, "run_end") != 1 {
		t.Fatal("батч дошёл до конца — должен быть run_end")
	}
	if countRunEvents(events, "run_failed") != 0 {
		t.Fatal("батч с частичными abort не должен писать run_failed")
	}
}

// latestRunStats — каталог единственного рана в runsDir.
func latestRunStats(t *testing.T, runsDir string) (RunStats, error) {
	t.Helper()
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return RunStats{}, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].IsDir() {
			return RunStats{RunDir: filepath.Join(runsDir, entries[i].Name())}, nil
		}
	}
	return RunStats{}, errors.New("в runsDir нет ни одного рана")
}
