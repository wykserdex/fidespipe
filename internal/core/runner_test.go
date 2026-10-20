package core

// Интеграционные тесты раннера: реальные subprocess-фикстуры из testdata/plugins.
// Каждый тест = один сценарий из PROTOCOL.md.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/gate"
)

const fxPlugins = "testdata/plugins/"

func sec(n int) Duration  { return Duration{Duration: time.Duration(n) * time.Second} }
func msec(n int) Duration { return Duration{Duration: time.Duration(n) * time.Millisecond} }

// counterPathFor — где хранит счётчик попыток плагин retry_flaky.
//
// Путь задаётся через RETRY_COUNTER и лежит ВНЕ каталога плагина: доверенность
// определяется хэшем содержимого каталога, и запись _counter рядом с main.py
// меняла бы хэш и отзывала доверие у плагина, который только что отработал.
func counterPathFor(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "retry_counter")
	t.Setenv("RETRY_COUNTER", p)
	return p
}

func resetCounter(t *testing.T, plugin string) {
	t.Helper()
	p := filepath.Join(fxPlugins, plugin, "_counter")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func readContextSnapshot(t *testing.T, runDir string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(runDir, "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("context.json не JSON: %v", err)
	}
	return parsed
}

// ── Happy path: два плагина, выход первого виден второму ───────────────

func TestRunHappyPath(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_happy",
			Input: map[string]interface{}{"value": "hello"},
			Steps: []Step{
				{ID: "echo", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5)},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("ран упал: %v", err)
	}
	if stats.OK != 1 || stats.Aborted != 0 {
		t.Fatalf("статы: %+v", stats)
	}

	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})
	echo := steps["echo"].(map[string]interface{})
	if echo["value"] != "hello" {
		t.Fatalf("выход шага не в неймспейсе шага: %v", steps)
	}
}

// ── foreach: stop останавливает элемент, не ран (PROTOCOL §6) ──────────

func TestRunForeachItemScope(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:        "t_foreach",
			Input:       map[string]interface{}{"values": []interface{}{"ok1", "bad", "ok2"}},
			Foreach:     "input.values",
			ForeachItem: "value",
			Steps: []Step{
				{ID: "check", Plugin: fxPlugins + "failer", OnError: "stop", Timeout: sec(5)},
				{ID: "echo", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5)},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("доменная stop не должна валить ран: %v", err)
	}
	if stats.OK != 2 || stats.Aborted != 1 {
		t.Fatalf("ожидалось ok=2 aborted=1, got %+v", stats)
	}

	events := readEvents(t, stats.RunDir)
	if countEvents(events, "item_aborted") != 1 {
		t.Fatalf("item_aborted должен быть ровно один: %v", events)
	}
	// у битого элемента второй шаг НЕ запускался: echo-шагов ровно два
	stepStartsEcho := 0
	for _, e := range events {
		if e["type"] == "step_start" && e["step"] == "echo" {
			stepStartsEcho++
		}
	}
	if stepStartsEcho != 2 {
		t.Fatalf("шаг после stop выполнился для aborted-элемента: %d", stepStartsEcho)
	}
}

// ── skip: цепочка продолжается, optional-вход потребителя пуст ─────────

func TestRunSkipPolicy(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_skip",
			Input: map[string]interface{}{"value": "bad"},
			Steps: []Step{
				{ID: "producer", Plugin: fxPlugins + "failer", OnError: "skip", Timeout: sec(5)},
				{ID: "consumer", Plugin: fxPlugins + "consumer_opt", OnError: "stop", Timeout: sec(5)},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("skip не должен валить ран: %v", err)
	}
	if stats.OK != 1 {
		t.Fatalf("статы: %+v", stats)
	}

	events := readEvents(t, stats.RunDir)
	if countEvents(events, "step_skipped") != 1 {
		t.Fatalf("нет step_skipped: %v", events)
	}

	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})
	if _, exists := steps["producer"]; exists {
		t.Fatal("skip-нутый шаг не должен писать в контекст")
	}
	consumer := steps["consumer"].(map[string]interface{})
	if consumer["got"] != "<none>" {
		t.Fatalf("optional-вход должен был отсутствовать: %v", consumer)
	}
}

// ── retry: retryable-ошибка успевает с 3-й попытки ─────────────────────

func TestRunRetrySucceeds(t *testing.T) {
	requirePython(t)
	counterPathFor(t)
	resetCounter(t, "retry_flaky")
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_retry",
			Input: map[string]interface{}{},
			Steps: []Step{
				{
					ID: "flaky", Plugin: fxPlugins + "retry_flaky",
					OnError: "retry", Timeout: sec(5),
					Retry: &Retry{Attempts: 3, Delay: msec(1), Backoff: "fixed"},
				},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("retry должен был успеться: %v", err)
	}
	if stats.OK != 1 {
		t.Fatalf("статы: %+v", stats)
	}
	events := readEvents(t, stats.RunDir)
	if n := countEvents(events, "step_start"); n != 3 {
		t.Fatalf("попыток должно быть 3, got %d", n)
	}
}

// ── retry: исчерпание = stop (PROTOCOL §6) ─────────────────────────────

func TestRunRetryExhaustedIsStop(t *testing.T) {
	requirePython(t)
	counterPathFor(t)
	resetCounter(t, "retry_flaky")
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_retry_fail",
			Input: map[string]interface{}{},
			Steps: []Step{
				{
					ID: "flaky", Plugin: fxPlugins + "retry_flaky",
					OnError: "retry", Timeout: sec(5),
					Retry: &Retry{Attempts: 2, Delay: msec(1)}, // а плагину нужно 3
				},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("исчерпанный retry = доменный stop, не ошибка рана: %v", err)
	}
	if stats.OK != 0 || stats.Aborted != 1 {
		t.Fatalf("статы: %+v", stats)
	}
	events := readEvents(t, stats.RunDir)
	if n := countEvents(events, "step_start"); n != 2 {
		t.Fatalf("попыток должно быть 2, got %d", n)
	}
	if countEvents(events, "step_failed") != 1 {
		t.Fatal("исчерпание retry должно фиксироваться step_failed")
	}
}

// ── не-retryable доменная ошибка НЕ ретраится ──────────────────────────

func TestRunNonRetryableStaysSingleAttempt(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_noretry",
			Input: map[string]interface{}{"value": "bad"},
			Steps: []Step{
				{
					ID: "f", Plugin: fxPlugins + "failer",
					OnError: "retry", Timeout: sec(5),
					Retry: &Retry{Attempts: 5, Delay: msec(1)},
				},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Aborted != 1 {
		t.Fatalf("статы: %+v", stats)
	}
	events := readEvents(t, stats.RunDir)
	if n := countEvents(events, "step_start"); n != 1 {
		t.Fatalf("не-retryable ошибка должна давать 1 попытку, got %d", n)
	}
}

// ── платформенная ошибка стопит весь ран, даже в foreach ───────────────

func TestRunPlatformErrorStopsWholeRun(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:        "t_crash",
			Input:       map[string]interface{}{"values": []interface{}{"a", "b"}},
			Foreach:     "input.values",
			ForeachItem: "value",
			Steps: []Step{
				{ID: "crash", Plugin: fxPlugins + "crasher", OnError: "skip", Timeout: sec(5)},
			},
		},
	}
	_, err := Run(pf, NewEngine(), quietOpts(t))
	if err == nil {
		t.Fatal("платформенная ошибка обязана стопить ран (on_error=skip не спасает)")
	}
	if !strings.Contains(err.Error(), "платформенная") {
		t.Fatalf("ошибка: %v", err)
	}
}

// ── нарушение контракта = платформенная ошибка (PROTOCOL §5) ───────────

func TestRunContractViolationStopsRun(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_breaker",
			Input: map[string]interface{}{},
			Steps: []Step{
				{ID: "breaker", Plugin: fxPlugins + "contract_breaker", OnError: "skip", Timeout: sec(5)},
			},
		},
	}
	_, err := Run(pf, NewEngine(), quietOpts(t))
	if err == nil || !strings.Contains(err.Error(), "контракт") {
		t.Fatalf("ожидалась ошибка контракта, got: %v", err)
	}
}

// ── незадекларированные поля отбрасываются с warning ───────────────────

func TestRunLeakerFieldsDropped(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_leaker",
			Input: map[string]interface{}{},
			Steps: []Step{
				{ID: "leak", Plugin: fxPlugins + "leaker", OnError: "stop", Timeout: sec(5)},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	snap := readContextSnapshot(t, stats.RunDir)
	leak := snap["steps"].(map[string]interface{})["leak"].(map[string]interface{})
	if _, exists := leak["junk"]; exists {
		t.Fatal("незадекларированное поле попало в контекст!")
	}
	if leak["value"] != "x" {
		t.Fatalf("задекларированное поле потерялось: %v", leak)
	}
	events := readEvents(t, stats.RunDir)
	if countEvents(events, "contract_warning") != 1 {
		t.Fatal("должно быть предупреждение contract_warning")
	}
}

// ── нарушение протокола = платформенная ошибка ─────────────────────────

func TestRunBadProtocolStopsRun(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_badproto",
			Input: map[string]interface{}{},
			Steps: []Step{
				{ID: "bad", Plugin: fxPlugins + "bad_proto", OnError: "skip", Timeout: sec(5)},
			},
		},
	}
	_, err := Run(pf, NewEngine(), quietOpts(t))
	if err == nil || !strings.Contains(err.Error(), "протокол") {
		t.Fatalf("ожидалось нарушение протокола, got: %v", err)
	}
}

// ── human_gate: auto-accept в CI через Run целиком ─────────────────────

func TestRunGateAutoAccept(t *testing.T) {
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_gate",
			Input: map[string]interface{}{"value": "hello"},
			Steps: []Step{
				{ID: "review", Plugin: "core/human_gate", Actions: []string{"accept", "reject"}},
			},
		},
	}
	opts := quietOpts(t)
	opts.Yes = true
	stats, err := Run(pf, NewEngine(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OK != 1 {
		t.Fatalf("статы: %+v", stats)
	}
	events := readEvents(t, stats.RunDir)
	if countEvents(events, "gate_decision") != 1 {
		t.Fatal("решение гейта должно быть в журнале")
	}
}

// ── human_gate: reject внутри рана → abort_item ────────────────────────

func TestRunGateRejectAborts(t *testing.T) {
	newStdin(t, "r\n")
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_gate_reject",
			Input: map[string]interface{}{},
			Steps: []Step{
				{ID: "review", Plugin: "core/human_gate"},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Aborted != 1 {
		t.Fatalf("reject должен аборти́ть элемент: %+v", stats)
	}
}

type scriptedGateUI struct {
	answers []string
	index   int
}

func (u *scriptedGateUI) ReadLine() (string, error) {
	if u.index >= len(u.answers) {
		return "", os.ErrClosed
	}
	answer := u.answers[u.index]
	u.index++
	return answer, nil
}

func TestRunGateRejectResumeRetriesItem(t *testing.T) {
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_gate_resume",
			Input: map[string]interface{}{},
			Steps: []Step{{ID: "review", Plugin: "core/human_gate"}},
		},
	}
	runsDir := t.TempDir()
	firstUI := &scriptedGateUI{answers: []string{"r\n"}}
	firstOpts := quietOpts(t)
	firstOpts.RunsDir = runsDir
	firstOpts.RunID = "resume-gate"
	firstOpts.GateUI = func(*Step) gate.GateUI { return firstUI }
	firstStats, err := Run(pf, NewEngine(), firstOpts)
	if err != nil {
		t.Fatal(err)
	}
	if firstStats.Aborted != 1 {
		t.Fatalf("unexpected first stats: %+v", firstStats)
	}

	secondUI := &scriptedGateUI{answers: []string{"a\n"}}
	secondOpts := quietOpts(t)
	secondOpts.RunsDir = runsDir
	secondOpts.Resume = filepath.Base(firstStats.RunDir)
	secondOpts.GateUI = func(*Step) gate.GateUI { return secondUI }
	secondStats, err := Run(pf, NewEngine(), secondOpts)
	if err != nil {
		t.Fatal(err)
	}
	if secondStats.OK != 1 || secondStats.Aborted != 0 {
		t.Fatalf("unexpected resume stats: %+v", secondStats)
	}
	if secondUI.index != 1 {
		t.Fatalf("resume did not reopen the gate: %d", secondUI.index)
	}
}

// ── v0.16: secrets — без env-переменной ран не стартует (до любого эффекта) ──

func TestRunSecretsMissing(t *testing.T) {
	os.Unsetenv("FIDESPIPE_TEST_SECRET_DO_NOT_SET")
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:    "t_secrets",
			Input:   map[string]interface{}{},
			Secrets: []string{"FIDESPIPE_TEST_SECRET_DO_NOT_SET"},
			Steps: []Step{
				{ID: "echo", Plugin: fxPlugins + "echo_ok", OnError: "stop"},
			},
		},
	}
	_, err := Run(pf, NewEngine(), quietOpts(t))
	if err == nil {
		t.Fatal("ожидается ошибка: ран без secrets")
	}
	if !strings.Contains(err.Error(), "FIDESPIPE_TEST_SECRET_DO_NOT_SET") {
		t.Fatalf("ошибка должна содержать имя ключа: %v", err)
	}
}
