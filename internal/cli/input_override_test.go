package cli

import (
	"testing"

	"github.com/wykserdex/fidespipe/internal/core"
)

// TestApplyInputOverrides — вписанное человеком значение попадает во вход.
//
// Это путь «принёс файл»: человек знает путь к своему файлу и не знает, что
// такое YAML. Значение переопределяет вход пресета, а не добавляет новый.
func TestApplyInputOverrides(t *testing.T) {
	pf := &core.PipelineFile{}
	pf.Pipeline.Input = map[string]interface{}{
		"file":   "plugins/community/pefile/fixtures/sample.dll",
		"emails": []interface{}{"old@example.com"},
	}
	pf.Pipeline.Foreach = "input.emails"

	if err := applyInputOverrides(pf, map[string]string{
		"file":   "C:\\tmp\\my.dll",
		"emails": "a@example.com\nb@example.com",
	}); err != nil {
		t.Fatalf("подстановка: %v", err)
	}
	if got := pf.Pipeline.Input["file"]; got != "C:\\tmp\\my.dll" {
		t.Errorf("file = %v, ждали указанный путь", got)
	}
	// Перебираемое поле — список: значение режется по строкам, иначе один
	// путь стал бы одним элементом списка из двух.
	list, ok := pf.Pipeline.Input["emails"].([]string)
	if !ok {
		t.Fatalf("emails = %T, ждали список строк", pf.Pipeline.Input["emails"])
	}
	if len(list) != 2 || list[0] != "a@example.com" || list[1] != "b@example.com" {
		t.Errorf("emails = %v, ждали два значения", list)
	}
}

// TestApplyInputOverridesRejectsUnknown — неизвестный вход отвергается.
//
// Иначе опечатка в ключе выглядела бы как «проверилось и ничего не нашлось».
func TestApplyInputOverridesRejectsUnknown(t *testing.T) {
	pf := &core.PipelineFile{}
	pf.Pipeline.Input = map[string]interface{}{"file": "x"}
	if err := applyInputOverrides(pf, map[string]string{"typo": "y"}); err == nil {
		t.Fatal("неизвестный вход должен быть отвергнут")
	}
}
