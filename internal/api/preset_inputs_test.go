package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// TestPresetInputsDescribesWhatToType — пресет описывает свои входы.
//
// Зачем: «проверить свою почту» не должно требовать правки YAML. Человек
// вписывает адрес в форму, поэтому консоли нужно знать, что пресет просит:
// имя входа, вид (список или одно значение) и формат элемента.
func TestPresetInputsDescribesWhatToType(t *testing.T) {
	pipelines := t.TempDir()
	preset := `format_version: "0.1"

pipeline:
  name: email_check
  network: allow
  input:
    emails:
      - "test@example.com"
    note: "разовый адрес"
  foreach: input.emails
  foreach_item: item
  item_type: string
  item_format: email
  steps:
    - id: syntax
      plugin: plugins/official/syntax_mx_checker
`
	if err := os.WriteFile(filepath.Join(pipelines, "email_check.yaml"), []byte(preset), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(t.TempDir(), pipelines, pipelines)

	inputs := srv.presetInputs("email_check.yaml")
	if len(inputs) != 2 {
		t.Fatalf("входов описано: %d, ждали 2: %+v", len(inputs), inputs)
	}
	byName := map[string]map[string]interface{}{}
	for _, in := range inputs {
		byName[in["name"].(string)] = in
	}
	emails, ok := byName["emails"]
	if !ok {
		t.Fatalf("входа emails нет: %+v", byName)
	}
	if emails["kind"] != "list" {
		t.Errorf("kind для списка: %v, ждали list", emails["kind"])
	}
	if emails["format"] != "email" {
		t.Errorf("format: %v, ждали email", emails["format"])
	}
	if emails["example"] != "test@example.com" {
		t.Errorf("example: %v", emails["example"])
	}
	note, ok := byName["note"]
	if !ok {
		t.Fatalf("входа note нет: %+v", byName)
	}
	if note["kind"] != "value" {
		t.Errorf("kind для одного значения: %v, ждали value", note["kind"])
	}
}

// TestApplyInputOverride — вписанные значения подставляются в пресет.
//
// Переопределяются только объявленные входы: неизвестный ключ — ошибка, а не
// тихое «проверилось и ничего не нашлось».
func TestApplyInputOverride(t *testing.T) {
	pf := &pipeline.PipelineFile{}
	pf.Pipeline.Input = map[string]interface{}{"emails": []interface{}{"old@example.com"}}

	if err := applyInputOverride(pf, map[string]interface{}{
		"emails": []interface{}{"new@example.com"},
	}); err != nil {
		t.Fatalf("подстановка объявленного входа: %v", err)
	}
	got, _ := pf.Pipeline.Input["emails"].([]interface{})
	if len(got) != 1 || got[0] != "new@example.com" {
		t.Fatalf("значение не подставлено: %+v", pf.Pipeline.Input["emails"])
	}

	if err := applyInputOverride(pf, map[string]interface{}{"typo": "x"}); err == nil {
		t.Fatal("неизвестный вход должен быть отвергнут")
	}
}
