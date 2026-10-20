package core

// v0.20: управляющий поток на уровне шага — when и foreach.

import (
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// ── when: условие истинно → шаг выполняется ────────────────────────────

func TestRunWhenTrue(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_when_true",
			Input: map[string]interface{}{"value": "hello"},
			Steps: []Step{
				{ID: "echo", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5)},
				{ID: "again", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5),
					Bind: map[string]string{"value": "steps.echo.value"},
					When: pipeline.When{Path: "steps.echo.value", Op: pipeline.OpTruthy}},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("ран упал: %v", err)
	}
	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})
	if _, ok := steps["again"]; !ok {
		t.Fatalf("шаг с истинным условием не выполнился: %v", steps)
	}
}

// ── when: условие ложно → шаг пропущен, вывода нет ─────────────────────

func TestRunWhenFalse(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_when_false",
			Input: map[string]interface{}{"value": "hello"},
			Steps: []Step{
				{ID: "echo", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5)},
				{ID: "again", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5),
					Bind: map[string]string{"value": "steps.echo.value"},
					When: pipeline.When{Path: "steps.echo.value", Op: pipeline.OpEq, Value: "другое"}},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("ран упал: %v", err)
	}
	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})
	if _, ok := steps["again"]; ok {
		t.Fatalf("шаг с ложным условием выполнился: %v", steps)
	}
	events := readEvents(t, stats.RunDir)
	if countEvents(events, "step_skipped") != 1 {
		t.Fatalf("ожидался ровно один step_skipped: %v", events)
	}
}

// ── when: числовое сравнение (gt) ──────────────────────────────────────

func TestRunWhenNumeric(t *testing.T) {
	requirePython(t)
	mk := func(n interface{}) *PipelineFile {
		return &PipelineFile{
			FormatVersion: PlatformAPI,
			Pipeline: Pipeline{
				Name:  "t_when_num",
				Input: map[string]interface{}{"value": "x", "n": n},
				Steps: []Step{
					{ID: "echo", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5),
						When: pipeline.When{Path: "input.n", Op: pipeline.OpGt, Value: 5}},
				},
			},
		}
	}
	for _, c := range []struct {
		n    interface{}
		want bool
	}{
		{float64(10), true},
		{float64(5), false},
		{float64(1), false},
	} {
		stats, err := Run(mk(c.n), NewEngine(), quietOpts(t))
		if err != nil {
			t.Fatalf("n=%v: %v", c.n, err)
		}
		snap := readContextSnapshot(t, stats.RunDir)
		steps := snap["steps"].(map[string]interface{})
		if _, ok := steps["echo"]; ok != c.want {
			t.Errorf("n=%v: шаг выполнен=%v, ожидается %v", c.n, ok, c.want)
		}
	}
}

// ── foreach на шаге: per-item мини-цикл, _all-агрегат ──────────────────

func TestRunStepForeach(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_step_foreach",
			Input: map[string]interface{}{"values": []interface{}{"a", "b", "c"}},
			Steps: []Step{
				{ID: "echo", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5),
					Foreach: "input.values", ForeachItem: "value"},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("ран упал: %v", err)
	}
	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})

	all, ok := steps["echo_all"].([]interface{})
	if !ok || len(all) != 3 {
		t.Fatalf("echo_all: ожидался массив из 3, got %v", steps["echo_all"])
	}
	for i, want := range []string{"a", "b", "c"} {
		got := all[i].(map[string]interface{})["value"]
		if got != want {
			t.Errorf("echo_all[%d] = %v, want %v", i, got, want)
		}
	}
	// steps.echo — выход последней итерации
	last := steps["echo"].(map[string]interface{})["value"]
	if last != "c" {
		t.Errorf("echo (последняя итерация) = %v, want c", last)
	}
	events := readEvents(t, stats.RunDir)
	if n := countEvents(events, "foreach_item_start"); n != 3 {
		t.Errorf("foreach_item_start: %d, want 3", n)
	}
}

// ── foreach на шаге: on_error=stop останавливает весь ран ──────────────

func TestRunStepForeachStopAbortsRun(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_step_foreach_stop",
			Input: map[string]interface{}{"values": []interface{}{"ok", "bad", "ok"}},
			Steps: []Step{
				{ID: "check", Plugin: fxPlugins + "failer", OnError: "stop", Timeout: sec(5),
					Foreach: "input.values", ForeachItem: "value"},
			},
		},
	}
	_, err := Run(pf, NewEngine(), quietOpts(t))
	if err == nil {
		t.Fatal("ожидалось падение рана (on_error=stop в step-foreach)")
	}
	if !strings.Contains(err.Error(), "on_error=stop") {
		t.Fatalf("сообщение не про stop: %v", err)
	}
}

// ── foreach на шаге: on_error=skip пропускает элемент, цикл продолжается ─

func TestRunStepForeachSkipContinues(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_step_foreach_skip",
			Input: map[string]interface{}{"values": []interface{}{"ok1", "bad", "ok2"}},
			Steps: []Step{
				{ID: "check", Plugin: fxPlugins + "failer", OnError: "skip", Timeout: sec(5),
					Foreach: "input.values", ForeachItem: "value"},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("skip не должен валить ран: %v", err)
	}
	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})
	all, ok := steps["check_all"].([]interface{})
	if !ok || len(all) != 2 {
		t.Fatalf("check_all: ожидались 2 успешных результата, got %v", steps["check_all"])
	}
	_ = stats
}

// ── валидатор: правила v0.20 ───────────────────────────────────────────

func TestValidateV020Rules(t *testing.T) {
	cases := []struct {
		name     string
		pf       *PipelineFile
		wantErrs []string
	}{
		{
			name: "when: неизвестный оператор",
			pf: &PipelineFile{FormatVersion: "0.2", Pipeline: Pipeline{
				Name:  "t",
				Input: map[string]interface{}{"value": "x"},
				Steps: []Step{
					{ID: "s", Plugin: fxPlugins + "echo_ok", When: pipeline.When{Path: "input.value", Op: "between"}},
				},
			}},
			wantErrs: []string{"неизвестный оператор"},
		},
		{
			name: "when: читает из ещё не выполненного шага",
			pf: &PipelineFile{FormatVersion: "0.2", Pipeline: Pipeline{
				Name:  "t",
				Input: map[string]interface{}{"value": "x"},
				Steps: []Step{
					{ID: "a", Plugin: fxPlugins + "echo_ok", When: pipeline.When{Path: "steps.b.value", Op: pipeline.OpTruthy}},
					{ID: "b", Plugin: fxPlugins + "echo_ok"},
				},
			}},
			wantErrs: []string{"ещё не выполняется"},
		},
		{
			name: "foreach: путь не из input",
			pf: &PipelineFile{FormatVersion: "0.2", Pipeline: Pipeline{
				Name:  "t",
				Input: map[string]interface{}{"value": "x"},
				Steps: []Step{
					{ID: "s", Plugin: fxPlugins + "echo_ok", Foreach: "input.no_such"},
				},
			}},
			wantErrs: []string{"не найден в input"},
		},
		{
			name: "foreach + after_foreach",
			pf: &PipelineFile{FormatVersion: "0.2", Pipeline: Pipeline{
				Name:  "t",
				Input: map[string]interface{}{"values": []interface{}{"x"}},
				Steps: []Step{
					{ID: "s", Plugin: fxPlugins + "echo_ok", Foreach: "input.values", AfterForeach: true},
				},
			}},
			wantErrs: []string{"не сочетаются"},
		},
		{
			name: "foreach в параллельной группе",
			pf: &PipelineFile{FormatVersion: "0.2", Pipeline: Pipeline{
				Name:  "t",
				Input: map[string]interface{}{"values": []interface{}{"x"}},
				Steps: []Step{
					{ID: "s1", Plugin: fxPlugins + "echo_ok", ParallelGroup: "g", Foreach: "input.values"},
					{ID: "s2", Plugin: fxPlugins + "echo_ok", ParallelGroup: "g"},
				},
			}},
			wantErrs: []string{"не сочетается с parallel_group"},
		},
		{
			name: "гейт в параллельной группе",
			pf: &PipelineFile{FormatVersion: "0.2", Pipeline: Pipeline{
				Name:  "t",
				Input: map[string]interface{}{"value": "x"},
				Steps: []Step{
					{ID: "g1", Plugin: "core/human_gate", ParallelGroup: "g"},
					{ID: "s2", Plugin: fxPlugins + "echo_ok", ParallelGroup: "g"},
				},
			}},
			wantErrs: []string{"нельзя ставить в параллельную группу"},
		},
		{
			name: "группа не смежная",
			pf: &PipelineFile{FormatVersion: "0.2", Pipeline: Pipeline{
				Name:  "t",
				Input: map[string]interface{}{"value": "x"},
				Steps: []Step{
					{ID: "a", Plugin: fxPlugins + "echo_ok", ParallelGroup: "g"},
					{ID: "mid", Plugin: fxPlugins + "echo_ok"},
					{ID: "b", Plugin: fxPlugins + "echo_ok", ParallelGroup: "g"},
				},
			}},
			wantErrs: []string{"должны быть рядом"},
		},
		{
			name: "корректный параллельный блок — без ошибок",
			pf: &PipelineFile{FormatVersion: "0.2", Pipeline: Pipeline{
				Name:  "t",
				Input: map[string]interface{}{"value": "x"},
				Steps: []Step{
					{ID: "a", Plugin: fxPlugins + "echo_ok", ParallelGroup: "g"},
					{ID: "b", Plugin: fxPlugins + "echo_ok", ParallelGroup: "g"},
				},
			}},
			wantErrs: nil,
		},
	}
	for _, c := range cases {
		errs, _ := Validate(c.pf, NewEngine())
		joined := ""
		for _, e := range errs {
			joined += e + "\n"
		}
		if c.wantErrs == nil {
			if len(errs) > 0 {
				t.Errorf("%s: неожиданные ошибки: %s", c.name, joined)
			}
			continue
		}
		for _, want := range c.wantErrs {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: не нашёл «%s» в ошибках: %s", c.name, want, joined)
			}
		}
	}
}

// ── parallel_group: обе ветки отработали, барьер закрылся ─────────────

func TestRunParallelGroup(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_parallel",
			Input: map[string]interface{}{"value": "x"},
			Steps: []Step{
				{ID: "a", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5), ParallelGroup: "g"},
				{ID: "b", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5), ParallelGroup: "g"},
				{ID: "after", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5)},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("ран упал: %v", err)
	}
	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})
	for _, id := range []string{"a", "b", "after"} {
		if _, ok := steps[id]; !ok {
			t.Fatalf("выход шага %s отсутствует: %v", id, steps)
		}
	}
	events := readEvents(t, stats.RunDir)
	if countEvents(events, "parallel_start") != 1 || countEvents(events, "parallel_end") != 1 {
		t.Fatalf("ожидалось по одному parallel_start/end: %v", events)
	}
	// «after» должен быть после parallel_end (барьер)
	lastEnd, lastAfter := -1, -1
	for i, e := range events {
		if e["type"] == "parallel_end" {
			lastEnd = i
		}
		if e["type"] == "step_start" && e["step"] == "after" {
			lastAfter = i
		}
	}
	if lastEnd < 0 || lastAfter < 0 || lastAfter < lastEnd {
		t.Fatalf("послебарьерный шаг до parallel_end: end=%d after=%d", lastEnd, lastAfter)
	}
}

// ── parallel_group: stop в ветке → ран остановлен ──────────────────────

func TestRunParallelGroupStopAborts(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_parallel_stop",
			Input: map[string]interface{}{"value": "bad"},
			Steps: []Step{
				{ID: "a", Plugin: fxPlugins + "failer", OnError: "stop", Timeout: sec(5), ParallelGroup: "g"},
				{ID: "b", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5), ParallelGroup: "g"},
			},
		},
	}
	_, err := Run(pf, NewEngine(), quietOpts(t))
	if err == nil {
		t.Fatal("ожидалось падение рана (stop в параллельной группе)")
	}
	if !strings.Contains(err.Error(), "параллельная группа") {
		t.Fatalf("сообщение не про группу: %v", err)
	}
}

// ── parallel_group: when-false ветка пропущена, другая жива ────────────

func TestRunParallelGroupWhenSkipped(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_parallel_when",
			Input: map[string]interface{}{"value": "x"},
			Steps: []Step{
				{ID: "a", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5), ParallelGroup: "g",
					When: pipeline.When{Path: "input.never", Op: pipeline.OpTruthy}},
				{ID: "b", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5), ParallelGroup: "g"},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("ран упал: %v", err)
	}
	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})
	if _, ok := steps["a"]; ok {
		t.Errorf("ветка с ложным when выполнила выход: %v", steps)
	}
	if _, ok := steps["b"]; !ok {
		t.Errorf("вторая ветка потерялась: %v", steps)
	}
}
