package execution

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

func TestRunStepLoopMaxIterations(t *testing.T) {
	st := &pipeline.Step{
		ID:            "test_loop",
		Loop:          "parser",
		LoopCondition: "steps.parser.continue",
		MaxIterations: 3,
	}
	if st.MaxIterations != 3 {
		t.Error("MaxIterations не установлен")
	}
}

func TestRunStepLoopDefaultMaxIterations(t *testing.T) {
	st := &pipeline.Step{
		ID:   "test_loop",
		Loop: "parser",
	}
	if st.MaxIterations != 0 {
		t.Error("MaxIterations должен быть 0 по умолчанию")
	}
}

func TestMaxLoopIterationsConstant(t *testing.T) {
	if pipeline.MaxLoopIterations != 100 {
		t.Error("MaxLoopIterations должен быть 100")
	}
}

// writeLoopPlugin — плагин, который всегда просит следующую итерацию:
// выход continue=true, поэтому loop крутится до своего max_iterations.
func writeLoopPlugin(t *testing.T, dir, id string) string {
	t.Helper()
	d := filepath.Join(dir, id)
	if err := os.MkdirAll(d, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"id": id, "version": "0.1", "platform_api": "0.1",
		"runtime": map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":   map[string]interface{}{},
		"output":  map[string]interface{}{"continue": map[string]interface{}{"type": "boolean"}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "plugin.yaml"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	py := "import sys,json\njson.load(sys.stdin)\njson.dump({'status':'ok','output':{'continue':True}},sys.stdout)\n"
	if err := os.WriteFile(filepath.Join(d, "main.py"), []byte(py), 0644); err != nil {
		t.Fatal(err)
	}
	return d
}

// MaxTotalLoopBudget обещает «глобальный» бюджет, а проверялся он счётчиком,
// который обнулялся на каждый вызов runStepLoop. То есть N loop-шагов давали
// N*1000 запусков плагина за один ран. Тест ловит именно это: два шага по 100
// итераций при бюджете 100 обязаны упасть на втором шаге, а не пройти.
func TestLoopBudgetIsSharedAcrossSteps(t *testing.T) {
	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "plugins")
	writeLoopPlugin(t, pluginsDir, "looper")

	steps := make([]pipeline.Step, 0, 2)
	for _, id := range []string{"loop_a", "loop_b"} {
		steps = append(steps, pipeline.Step{
			ID: id, Plugin: filepath.Join(pluginsDir, "looper"),
			Loop: "parser", LoopCondition: "steps." + id + ".continue", MaxIterations: 100,
		})
	}
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline:      pipeline.Pipeline{Name: "loop_budget", Input: map[string]interface{}{}, Steps: steps},
	}
	opts := RunOptions{
		Quiet: true, RunsDir: filepath.Join(dir, "runs"),
		Trusted:    trustPluginsDir(t, pluginsDir),
		loopBudget: &loopBudgetCounter{limit: 100},
	}
	_, err := Run(pf, &mapEngine{}, opts)
	if err == nil {
		t.Fatal("два loop-шага по 100 итераций при бюджете 100 прошли: бюджет не общий")
	}
	if !strings.Contains(err.Error(), "бюджет") {
		t.Fatalf("ошибка не про бюджет итераций: %v", err)
	}
}

// Шаг, который укладывается в бюджет, обязан завершиться: счётчик не должен
// блокировать честный ран.
func TestLoopBudgetAllowsWithinLimit(t *testing.T) {
	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "plugins")
	writeLoopPlugin(t, pluginsDir, "looper")

	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "loop_ok",
			Input: map[string]interface{}{},
			Steps: []pipeline.Step{{
				ID: "loop_a", Plugin: filepath.Join(pluginsDir, "looper"),
				Loop: "parser", LoopCondition: "steps.loop_a.continue", MaxIterations: 10,
			}},
		},
	}
	opts := RunOptions{
		Quiet: true, RunsDir: filepath.Join(dir, "runs"),
		Trusted:    trustPluginsDir(t, pluginsDir),
		loopBudget: &loopBudgetCounter{limit: 100},
	}
	stats, err := Run(pf, &mapEngine{}, opts)
	if err != nil {
		t.Fatalf("ран в пределах бюджета упал: %v", err)
	}
	if stats.OK != 1 {
		t.Fatalf("stats.OK = %d, ожидался 1", stats.OK)
	}
}

// Новый ран начинается с нулевого бюджета: прошлый ран не должен есть лимит
// следующего, иначе 1000 итераций накапливались бы по всему проекту.
func TestLoopBudgetResetsBetweenRuns(t *testing.T) {
	budget := &loopBudgetCounter{}
	opts := RunOptions{loopBudget: budget}
	for i := 0; i < 50; i++ {
		if !opts.consumeLoopIteration() {
			t.Fatalf("итерация %d: бюджет исчерпан преждевременно", i+1)
		}
	}
	if got := opts.loopIterations(); got != 50 {
		t.Fatalf("loopIterations() = %d, ожидалось 50", got)
	}
	fresh := RunOptions{loopBudget: &loopBudgetCounter{}}
	if got := fresh.loopIterations(); got != 0 {
		t.Fatalf("новый бюджет = %d, ожидался 0", got)
	}
}

// RunOptions без заведённого бюджета (внешний вызов с нулевой структурой)
// обязан работать, а не падать: consumeLoopIteration возвращает true.
func TestLoopBudgetAbsentIsNoop(t *testing.T) {
	opts := RunOptions{}
	if !opts.consumeLoopIteration() {
		t.Fatal("без бюджета итерация не прошла")
	}
}
