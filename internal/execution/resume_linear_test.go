package execution

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
)

// writeFlakyPlugin — плагин, который ПАДАЕТ в первый запуск и проходит во
// второй.
//
// Механизм: плагин считает свои запуски в файле рядом с собой и на первый
// запуск выходит с кодом 1, на второй — с кодом 0. Так тест может отличить
// «шаг выполнен заново» от «шаг пропущен при resume».
func writeFlakyPlugin(t *testing.T, dir, name string, failFirst bool) string {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"id": name, "version": "0.1", "platform_api": "0.1",
		"runtime": map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":   map[string]interface{}{},
		"output":  map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(d, "plugin.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	py := `import json, os, sys
data = json.load(sys.stdin)
marker = os.path.join(os.path.dirname(os.path.abspath(__file__)), "runs.txt")
n = 0
if os.path.exists(marker):
    n = int(open(marker).read().strip() or "0")
n += 1
open(marker, "w").write(str(n))
if FAIL_FIRST and n == 1:
    json.dump({"status": "error", "error": {"code": "boom", "message": "первый запуск всегда падает", "retryable": False}}, sys.stdout)
    sys.exit(1)
json.dump({"status": "ok", "output": {"done": True, "runs": n}}, sys.stdout)
`
	failFlag := "False"
	if failFirst {
		failFlag = "True"
	}
	py = strings.Replace(py, "FAIL_FIRST", failFlag, 1)
	if err := os.WriteFile(filepath.Join(d, "main.py"), []byte(py), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

// stepRuns — сколько раз плагин выполнялся.
func stepRuns(t *testing.T, pluginDir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pluginDir, "runs.txt"))
	if err != nil {
		t.Fatalf("плагин ни разу не выполнялся: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(string(raw), "%d", &n); err != nil {
		t.Fatalf("счётчик запусков не разобран: %v", err)
	}
	return n
}

// TestLinearChainResumeSkipsDoneSteps — главный сценарий: линейная цепочка,
// где первый шаг прошёл, второй упал. Resume обязан НЕ выполнять первый шаг
// заново и продолжить со второго.
//
// До правки resume линейной цепочки начинался с нуля: линейная цепочка устроена
// как foreach с одним элементом, поэтому курсор по элементу всегда ноль, и
// первый шаг выполнялся повторно. Для шага с побочным эффектом (отправка
// письма, платёж) это двойное действие.
func TestLinearChainResumeSkipsDoneSteps(t *testing.T) {
	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "plugins")
	first := writeFlakyPlugin(t, pluginsDir, "first", false)
	second := writeFlakyPlugin(t, pluginsDir, "second", true)
	runsDir := filepath.Join(dir, "runs")

	pf := pipelineFileLinear(first, second)

	// Первый прогон: первый шаг проходит, второй падает.
	//
	// Падение шага с on_error: stop выражается НЕ ошибкой Run, а aborted в
	// статистике: ран завершается штатно, просто с остановленным элементом.
	st, err := Run(pf, &mapEngine{}, RunOptions{Yes: true, Quiet: true, RunID: "run1", RunsDir: runsDir, Trusted: trustPluginsDir(t, pluginsDir)})
	if err != nil {
		t.Fatalf("первый прогон: %v", err)
	}
	if st.Aborted == 0 {
		t.Fatal("первый прогон обязан остановиться на втором шаге")
	}
	if n := stepRuns(t, first); n != 1 {
		t.Fatalf("первый шаг выполнен %d раз, ожидался 1", n)
	}
	if n := stepRuns(t, second); n != 1 {
		t.Fatalf("второй шаг выполнен %d раз, ожидался 1", n)
	}

	// Resume: первый шаг обязан быть пропущен, второй — выполнен.
	if _, err := Run(pf, &mapEngine{}, RunOptions{Yes: true, Quiet: true, Resume: "run1", RunsDir: runsDir, Trusted: trustPluginsDir(t, pluginsDir)}); err != nil {
		t.Fatalf("resume не удался: %v", err)
	}
	if n := stepRuns(t, first); n != 1 {
		t.Errorf("первый шаг выполнен %d раз — на resume он выполнен заново, а должен быть пропущен", n)
	}
	if n := stepRuns(t, second); n != 2 {
		t.Errorf("второй шаг выполнен %d раз, ожидалось 2 (первый прогон + resume)", n)
	}
}

// TestLinearChainResumeRerunsStepMissingFromContext — шаг, который отработал,
// но чей выход НЕ попал в снапшот контекста, выполняется заново.
//
// Это случай «процесс убили, а не упали»: снапшот отстал, и пропуск шага без
// его выхода скормил бы дальше неполные данные. Лучше лишний раз выполнить
// шаг, чем продолжить с дырой в состоянии.
func TestLinearChainResumeRerunsStepMissingFromContext(t *testing.T) {
	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "plugins")
	first := writeFlakyPlugin(t, pluginsDir, "first", false)
	second := writeFlakyPlugin(t, pluginsDir, "second", true)
	runsDir := filepath.Join(dir, "runs")

	pf := pipelineFileLinear(first, second)
	st, err := Run(pf, &mapEngine{}, RunOptions{Yes: true, Quiet: true, RunID: "run1", RunsDir: runsDir, Trusted: trustPluginsDir(t, pluginsDir)})
	if err != nil {
		t.Fatalf("первый прогон: %v", err)
	}
	if st.Aborted == 0 {
		t.Fatal("первый прогон обязан остановиться на втором шаге")
	}

	// Снапшот отстал: выход первого шага убран из контекста.
	raw, err := os.ReadFile(filepath.Join(runsDir, "run1", "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ctxData map[string]interface{}
	if err := json.Unmarshal(raw, &ctxData); err != nil {
		t.Fatal(err)
	}
	// Ключ выхода шага в контексте — id шага («step1»), а не имя плагина.
	if steps, ok := ctxData["steps"].(map[string]interface{}); ok {
		delete(steps, "step1")
	}
	trimmed, _ := json.Marshal(ctxData)
	if err := os.WriteFile(filepath.Join(runsDir, "run1", "context.json"), trimmed, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(pf, &mapEngine{}, RunOptions{Yes: true, Quiet: true, Resume: "run1", RunsDir: runsDir, Trusted: trustPluginsDir(t, pluginsDir)}); err != nil {
		t.Fatalf("resume не удался: %v", err)
	}
	if n := stepRuns(t, first); n != 2 {
		t.Errorf("первый шаг выполнен %d раз — ожидалось 2: без выхода в снапшоте шаг обязан быть выполнен заново", n)
	}
}

// TestResumeStepCursorReadsJournal — курсор по шагам читается из журнала:
// в счёт идут только завершившиеся успешно.
func TestResumeStepCursorReadsJournal(t *testing.T) {
	dir := t.TempDir()
	j, err := journal.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	j.Event("step_start", map[string]interface{}{"step": "a", "attempt": 1})
	j.Event("step_end", map[string]interface{}{"step": "a", "status": "ok"})
	j.Event("step_start", map[string]interface{}{"step": "b", "attempt": 1})
	j.Event("step_end", map[string]interface{}{"step": "b", "status": "domain_error"})
	j.Event("step_start", map[string]interface{}{"step": "c", "attempt": 1})
	j.Event("step_end", map[string]interface{}{"step": "c", "status": "ok"})

	done, err := resumeStepCursor(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !done["a"] || !done["c"] {
		t.Errorf("в курсоре нет успешных шагов: %v", done)
	}
	if done["b"] {
		t.Error("в курсоре попал шаг с ошибкой — он выполнен не был")
	}
}

// pipelineFileLinear — линейная цепочка из двух шагов (без foreach).
func pipelineFileLinear(steps ...string) *pipeline.PipelineFile {
	ps := make([]pipeline.Step, 0, len(steps))
	for i, dir := range steps {
		ps = append(ps, pipeline.Step{
			ID:     "step" + string(rune('1'+i)),
			Plugin: dir,
			// on_error: stop — иначе упавший шаг пропускается, ран доходит до
			// конца и падать не на чем.
			OnError: "stop",
		})
	}
	return &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "linear_resume",
			Input: map[string]interface{}{},
			Steps: ps,
		},
	}
}
