package execution

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/runctx"
)

// resumeGatePipeline — пайплайн с шагом core/human_gate: гейт пишет выход
// через ctx.SetStep, то есть именно тот путь, на котором битый context.json
// (`steps: 5`) раньше ронял ран panic'ом. Авто-аппрув через --yes — без ввода.
func resumeGatePipeline() *pipeline.PipelineFile {
	return &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "resume_shape",
			Input: map[string]interface{}{"item": "x"},
			Steps: []pipeline.Step{{
				ID:     "review",
				Plugin: "core/human_gate",
				Form:   []pipeline.FormField{{Field: "input.item", Type: "string"}},
			}},
		},
	}
}

// writeContext — подменяет context.json в каталоге рана.
func writeContext(t *testing.T, runsDir, runID, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(runsDir, runID, "context.json"), []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
}

// seedRun — эталонный прогон: журнал с run_start и pipeline_hash для resume.
func seedRun(t *testing.T, pf *pipeline.PipelineFile, dir, runID string) {
	t.Helper()
	if _, err := Run(pf, permissiveEngine{}, RunOptions{Yes: true, Quiet: true, RunID: runID, RunsDir: dir}); err != nil {
		t.Fatalf("эталонный прогон: %v", err)
	}
}

// F-01: --resume не доверяет форме context.json. Битый/чужой context.json
// (`steps: 5`) обязан дать типизированную ошибку до исполнения, а не panic
// на unchecked type assertion.
func TestResumeRejectsForeignContextShape(t *testing.T) {
	cases := []struct {
		name  string
		shape string
		field string
	}{
		{"steps не объект", `{"steps":5}`, "steps"},
		{"input не объект", `{"input":5,"steps":{}}`, "input"},
		{"steps массив", `{"input":{},"steps":[]}`, "steps"},
		{"input строка", `{"input":"nope","steps":{}}`, "input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("resume упал на битом context.json: %v", r)
				}
			}()
			dir := t.TempDir()
			pf := resumeGatePipeline()
			seedRun(t, pf, dir, "shape-run")
			writeContext(t, dir, "shape-run", tc.shape)

			_, err := Run(pf, permissiveEngine{}, RunOptions{Yes: true, Quiet: true, Resume: "shape-run", RunsDir: dir})
			if err == nil {
				t.Fatalf("context.json %s принят без ошибки", tc.shape)
			}
			var shape *runctx.ShapeError
			if !errors.As(err, &shape) {
				t.Fatalf("ожидался *runctx.ShapeError, получено %T: %v", err, err)
			}
			if shape.Field != tc.field {
				t.Fatalf("поле %q, ожидалось %q", shape.Field, tc.field)
			}
			// ERRORS.md: прочие рантайм-сбои (резолв, resume) → run_error.
			if code := ErrorCode(err); code != "run_error" {
				t.Fatalf("код ошибки %q, ожидался run_error", code)
			}
			if !contains(err.Error(), "--resume shape-run") {
				t.Fatalf("в сообщении нет контекста resume: %v", err)
			}
		})
	}
}

// F-01: nil допускается и нормализуется. context.json без input (или с
// input: null) — нормальный resume: input становится пустым объектом, ран
// доходит до конца, accessors не паникуют.
func TestResumeNormalizesMissingContextNamespaces(t *testing.T) {
	cases := []struct{ name, shape string }{
		{"input отсутствует", `{"steps":{}}`},
		{"input null", `{"input":null,"steps":{}}`},
		{"контекст пустой", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("resume упал на нормализуемом context.json: %v", r)
				}
			}()
			dir := t.TempDir()
			pf := resumeGatePipeline()
			seedRun(t, pf, dir, "normalize-run")
			writeContext(t, dir, "normalize-run", tc.shape)

			if _, err := Run(pf, permissiveEngine{}, RunOptions{Yes: true, Quiet: true, Resume: "normalize-run", RunsDir: dir}); err != nil {
				t.Fatalf("нормализуемый context.json %s обязан резюмироваться: %v", tc.shape, err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "normalize-run", "context.json"))
			if err != nil {
				t.Fatal(err)
			}
			var snap map[string]interface{}
			if err := json.Unmarshal(raw, &snap); err != nil {
				t.Fatal(err)
			}
			for _, ns := range []string{"input", "steps"} {
				if _, ok := snap[ns].(map[string]interface{}); !ok {
					t.Fatalf("после resume %s = %#v, ожидался object", ns, snap[ns])
				}
			}
		})
	}
}

// F-01: нормальный resume не меняется — валидный context.json подхватывается
// как есть (шаг гейта видит данные input), resume завершается без ошибок.
func TestResumeWithValidContextKeepsWorking(t *testing.T) {
	dir := t.TempDir()
	pf := resumeGatePipeline()
	seedRun(t, pf, dir, "valid-run")
	writeContext(t, dir, "valid-run", `{"input":{"item":"y"},"steps":{"review":{"item":"y"}}}`)

	stats, err := Run(pf, permissiveEngine{}, RunOptions{Yes: true, Quiet: true, Resume: "valid-run", RunsDir: dir})
	if err != nil {
		t.Fatalf("валидный resume: %v", err)
	}
	if stats.OK != 2 {
		t.Fatalf("ожидались 2 обработанных элемента (эталонный + resume), получено %+v", stats)
	}
	data, err := journal.NewFilesystemStore(dir).LoadContext("valid-run")
	if err != nil {
		t.Fatal(err)
	}
	review, _ := data["steps"].(map[string]interface{})["review"].(map[string]interface{})
	if review["item"] != "y" {
		t.Fatalf("resume потерял выход шага: %#v", review)
	}
}
