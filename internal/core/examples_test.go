package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// Примеры из examples/ — документация, которую копируют. Дрейф между ними и
// рантаймом стоил реальных правок: пустое поле network стало означать deny, и
// шесть примеров с сетевыми плагинами перестали запускаться, при том что ни
// один тест их не проверял. Этот тест — именно та проверка, которой не было.
func TestExamplesAreValid(t *testing.T) {
	// Пути плагинов в примерах относительные, а тест живёт в internal/core.
	// Раньше здесь стоял os.Chdir в корень репозитория, но менять глобальный
	// каталог процесса в тесте — плохая идея: это ломает любой параллельный
	// тест и любой дочерний процесс, запущенный другим тестом. Вместо этого
	// пути переписываются в абсолютные, а движку указывается абсолютный
	// PluginsDir.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "examples"))
	if err != nil {
		t.Fatalf("examples недоступны: %v", err)
	}
	eng := NewEngine()
	eng.PluginsDir = filepath.Join(root, "plugins")
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(root, "examples", e.Name())
		pf, err := pipeline.LoadPipelineFile(path)
		if err != nil {
			t.Errorf("%s: не читается: %v", e.Name(), err)
			continue
		}
		for i := range pf.Pipeline.Steps {
			ref := pf.Pipeline.Steps[i].Plugin
			if ref == "" || pipeline.IsBuiltin(ref) || filepath.IsAbs(ref) {
				continue
			}
			pf.Pipeline.Steps[i].Plugin = filepath.Join(root, ref)
		}
		checked++
		for _, is := range pipeline.ValidateIssues(pf, eng) {
			if is.Severity == "error" {
				t.Errorf("%s: %s: %s (подсказка: %s)", e.Name(), is.Code, is.Message, is.Hint)
			}
		}
	}
	if checked == 0 {
		t.Fatal("не найдено ни одного примера — тест ничего не проверяет")
	}
	t.Logf("проверено примеров: %d", checked)
}
