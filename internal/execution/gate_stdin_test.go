package execution

import (
	"io"
	"os"
	"testing"

	"github.com/wykserdex/fidespipe/internal/gate"
	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// twoGatePipeline — два human_gate подряд в одном ране. Именно этот сценарий
// ломался: на каждый гейт создавался свой bufio.Reader на os.Stdin, и при
// stdin из pipe первый гейт забирал в буфер оба ответа разом, а второй
// получал EOF («ввод закрыт — гейт: стоп»). С TTY баг не виден: там строки
// печатает человек, по одной за раз.
func twoGatePipeline() *pipeline.PipelineFile {
	return &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "two_gates",
			Input: map[string]interface{}{"item": "x"},
			Steps: []pipeline.Step{
				{ID: "gate_one", Plugin: "core/human_gate", Form: []pipeline.FormField{{Field: "input.item", Type: "string"}}},
				{ID: "gate_two", Plugin: "core/human_gate", Form: []pipeline.FormField{{Field: "input.item", Type: "string"}}},
			},
		},
	}
}

// pipeForeachGatePipeline — ОДИН гейт, но ран заходит в него по разу на элемент
// (pipeline foreach). Это худший случай старого кода: gate.NewService()
// вызывался из runStep, а runStep вызывается в цикле по элементам, то есть
// буферизованный ввод пересоздавался N раз.
func pipeForeachGatePipeline() *pipeline.PipelineFile {
	return &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:    "two_gates_foreach",
			Input:   map[string]interface{}{"items": []interface{}{"one", "two"}},
			Foreach: "input.items",
			Steps: []pipeline.Step{{
				ID: "review", Plugin: "core/human_gate",
				Form: []pipeline.FormField{{Field: "input.item", Type: "string"}},
			}},
		},
	}
}

// pipeStdin — подменить os.Stdin концом pipe (не TTY) с готовыми ответами.
// Возвращает read end, чтобы тест мог проверить, что ран его не закрыл.
//
// os.Stdin — глобальная переменная процесса, поэтому подмена безопасна только
// потому, что в пакете нет t.Parallel() (проверено: 0 совпадений). Если
// кто-то добавит параллельный тест, подменять надо через gate.GateUI.
func pipeStdin(t *testing.T, answers string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := io.WriteString(w, answers); err != nil {
		t.Fatalf("запись в pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close write end: %v", err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		r.Close()
	})
	return r
}

// gateDecisions — решения гейтов из журнала рана (type, step, action).
func gateDecisions(t *testing.T, dir string) []map[string]interface{} {
	t.Helper()
	events, err := journal.NewReader(dir).Events()
	if err != nil {
		t.Fatalf("чтение журнала: %v", err)
	}
	var out []map[string]interface{}
	for _, ev := range events {
		if ev["type"] == "gate_decision" {
			out = append(out, ev)
		}
	}
	return out
}

// Регрессия: stdin из pipe, НЕСКОЛЬКО гейтов подряд, на каждый — свой ответ.
// До правки второй гейт получал EOF (ответы съедены буфером первого) и ран
// отдавал abort_item; TTY-тесты этого не видели.
//
// Уровень теста — верхний: настоящий execution.Run с настоящим
// gate.NewStdinUI(); единственная подмена — os.Stdin на конец pipe, то есть
// ровно то окружение, в котором баг и живёт (не TTY).
func TestGateStdinPipe(t *testing.T) {
	cases := []struct {
		name  string
		pf    *pipeline.PipelineFile
		ok    int
		gates int
	}{
		{"два гейта подряд", twoGatePipeline(), 1, 2},
		{"гейт в цикле foreach", pipeForeachGatePipeline(), 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdin := pipeStdin(t, "accept\naccept\n")
			stats, err := Run(tc.pf, permissiveEngine{}, RunOptions{Quiet: true, RunsDir: t.TempDir()})
			if err != nil {
				t.Fatalf("ран упал: %v", err)
			}
			if stats.OK != tc.ok || stats.Aborted != 0 {
				t.Fatalf("все гейты должны быть приняты с piped stdin: ok=%d (ожидалось %d) aborted=%d (журнал %s)", stats.OK, tc.ok, stats.Aborted, stats.RunDir)
			}
			decisions := gateDecisions(t, stats.RunDir)
			if len(decisions) != tc.gates {
				t.Fatalf("ожидались %d решения гейта, получено %d: %v", tc.gates, len(decisions), decisions)
			}
			for _, d := range decisions {
				if d["step"] == nil {
					t.Fatalf("решение без шага: %v", d)
				}
				if d["action"] != "accept" {
					t.Fatalf("шаг %v: решение %v, ожидался accept", d["step"], d["action"])
				}
			}
			// Жизненный цикл: stdin принадлежит процессу, ран его не закрывает.
			// На закрытом дескрипторе Stat даёт ошибку — это и есть проверка.
			if _, err := stdin.Stat(); err != nil {
				t.Fatalf("ран закрыл os.Stdin: %v", err)
			}
		})
	}
}

// scriptedUI — GateUI, который помнит свой экземпляр и номер ReadLine.
type scriptedUI struct {
	lines []string
	reads int
}

func (u *scriptedUI) ReadLine() (string, error) {
	if u.reads >= len(u.lines) {
		return "", io.EOF
	}
	s := u.lines[u.reads]
	u.reads++
	return s, nil
}

// uiFactory — выдаёт заведомо СВЕЖИЙ экземпляр на каждый вызов. Ран, который
// создаёт терминальный UI на каждый гейт (как было до правки), взял бы два
// экземпляра; ран, который берёт UI один на прогон, — один.
type uiFactory struct {
	lines   []string
	created []*scriptedUI
}

func (f *uiFactory) new() gate.GateUI {
	ui := &scriptedUI{lines: f.lines}
	f.created = append(f.created, ui)
	return ui
}

// Инвариант владения: терминальный ввод заводится ОДИН раз на ран и живёт
// до конца прогона. Плюс второй ран в том же процессе получает свой
// экземпляр — общий на процесс UI был бы другой утечкой.
func TestGateStdinOwnedByRun(t *testing.T) {
	f := &uiFactory{lines: []string{"accept", "accept"}}
	dir := t.TempDir()

	stats, err := Run(twoGatePipeline(), permissiveEngine{}, RunOptions{Quiet: true, RunsDir: dir, gateUI: f.new()})
	if err != nil {
		t.Fatalf("ран упал: %v", err)
	}
	if stats.OK != 1 || stats.Aborted != 0 {
		t.Fatalf("ok=%d aborted=%d, ожидался один принятый элемент", stats.OK, stats.Aborted)
	}
	if len(f.created) != 1 {
		t.Fatalf("UI ввода создано %d раз на ран, ожидался один (новый bufio.Reader на каждый гейт съедает буфер)", len(f.created))
	}
	if f.created[0].reads != 2 {
		t.Fatalf("из одного UI прочитано %d строк, ожидалось 2 (по одной на гейт)", f.created[0].reads)
	}

	// Второй ран в том же процессе — свой экземпляр.
	f2 := &uiFactory{lines: []string{"accept", "accept"}}
	if _, err := Run(twoGatePipeline(), permissiveEngine{}, RunOptions{Quiet: true, RunsDir: t.TempDir(), gateUI: f2.new()}); err != nil {
		t.Fatalf("второй ран упал: %v", err)
	}
	if len(f2.created) != 1 {
		t.Fatalf("второй ран: UI создано %d раз, ожидался один", len(f2.created))
	}
}
