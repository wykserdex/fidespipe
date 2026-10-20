package core

// v0.23: контракт рантайма — интеграция (ран реально падает на «вруне»).

import (
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// Плагин врёт тип (фикстура type_drifter: обещает string, возвращает 42).
// До v0.23 такой ран завершался ok=1 — неправильный тип утекал downstream.
func TestRunOutputContractViolation(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_contract_out",
			Input: map[string]interface{}{},
			Steps: []Step{
				{ID: "drift", Plugin: fxPlugins + "type_drifter", OnError: "stop", Timeout: sec(5)},
				{ID: "sink", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5),
					Bind: map[string]string{"value": "steps.drift.value"}},
			},
		},
	}
	_, err := Run(pf, NewEngine(), quietOpts(t))
	if err == nil {
		t.Fatal("ожидалось падение рана: плагин нарушил контракт (string → number)")
	}
	if !strings.Contains(err.Error(), "нарушение контракта") {
		t.Fatalf("сообщение не про контракт: %v", err)
	}
}

// Симметричная дыра: upstream вернул string, вход downstream объявлен number.
// До v0.23 строка тихо проходила в плагин, обещанный работать с числами.
func TestRunInputContractViolation(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_contract_in",
			Input: map[string]interface{}{"value": "42"}, // строка «42», не число
			Steps: []Step{
				{ID: "echo", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5)},
				{ID: "num", Plugin: fxPlugins + "num_only", OnError: "stop", Timeout: sec(5),
					Bind: map[string]string{"n": "steps.echo.value"}},
			},
		},
	}
	// v0.9 (ERRORS.md: contract_input): вход не прошёл контракт — строка не
	// уходит в плагин (как в v0.23), но падает элемент, а не весь ран.
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("contract_input не должен ронять ран: %v", err)
	}
	if stats.Aborted != 1 || stats.OK != 0 {
		t.Fatalf("ожидался aborted=1 ok=0, got %+v", stats)
	}
	assertContractInput(t, stats.RunDir, "нарушение контракта")
}

// Честный кейс: число на входе, число на выходе — контракт держится.
func TestRunContractHappy(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_contract_ok",
			Input: map[string]interface{}{"n": float64(5)},
			Steps: []Step{
				{ID: "num", Plugin: fxPlugins + "num_only", OnError: "stop", Timeout: sec(5)},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("честный ран упал: %v", err)
	}
	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})
	if steps["num"] == nil {
		t.Fatalf("шаг не выполнился: %v", steps)
	}
}

// when-условие на типе: number > 10 (проверка, что контрактные числа
// корректны для when/операторов)
func TestWhenNumberContracted(t *testing.T) {
	requirePython(t)
	pf := &PipelineFile{
		FormatVersion: PlatformAPI,
		Pipeline: Pipeline{
			Name:  "t_when_contract",
			Input: map[string]interface{}{"n": float64(15), "value": "hi"},
			Steps: []Step{
				{ID: "num", Plugin: fxPlugins + "num_only", OnError: "stop", Timeout: sec(5)},
				{ID: "show", Plugin: fxPlugins + "echo_ok", OnError: "stop", Timeout: sec(5),
					// value — string-порт, поэтому из input; when — по контрактному числу
					Bind: map[string]string{"value": "input.value"},
					When: pipeline.When{Path: "steps.num.n", Op: pipeline.OpGt, Value: float64(10)}},
			},
		},
	}
	stats, err := Run(pf, NewEngine(), quietOpts(t))
	if err != nil {
		t.Fatalf("ран упал: %v", err)
	}
	snap := readContextSnapshot(t, stats.RunDir)
	steps := snap["steps"].(map[string]interface{})
	if _, ok := steps["show"]; !ok {
		t.Fatalf("when (15 > 10) должен был пропустить шаг: %v", steps)
	}
}
