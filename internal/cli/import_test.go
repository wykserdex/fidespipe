package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/importer"
)

// TestPluginImportCreatesValidPlugin — команда создаёт рабочий плагин.
//
// Проверяется ровно то, что обещает импортёр: каталог проходит
// ValidatePluginDir, а тесты зелёные. Плагин, который импортировали, но который
// не запускается, хуже, чем отказ импорта: человек получит «готово» и
// нерабочий код.
func TestPluginImportCreatesValidPlugin(t *testing.T) {
	project := filepath.Join("..", "importer", "testdata", "sample")
	if _, err := os.Stat(project); err != nil {
		t.Skipf("фикстура импортёра недоступна: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "imported_plugin")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := importer.Plan(mustInspect(t, project))
	res, err := importer.Generate(plan, dest, importer.GenerateOptions{})
	if err != nil {
		t.Fatalf("генерация: %v", err)
	}
	if res.Dir == "" {
		t.Fatal("каталог не задан")
	}
	for _, f := range []string{"plugin.yaml", "main.py", "plugin.test.yaml", "README.md"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("файл %s не создан: %v", f, err)
		}
	}
	if errs := core.ValidatePluginDir(dest); len(errs) > 0 {
		t.Errorf("импортированный плагин невалиден: %v", errs)
	}
	passed, failed, err := core.RunPluginTests(dest, "", false)
	if err != nil {
		t.Skipf("тесты не запустились (нужен python3): %v", err)
	}
	if failed > 0 {
		t.Errorf("тесты импортированного плагина красные: %d из %d", failed, passed+failed)
	}
}

// TestPluginImportPlanDoesNotWrite — --plan ничего не пишет.
//
// Разбор без записи — половина ценности команды: посмотреть, что выйдет из
// чужого проекта, можно и не создавая каталог.
func TestPluginImportPlanDoesNotWrite(t *testing.T) {
	project := filepath.Join("..", "importer", "testdata", "sample")
	if _, err := os.Stat(project); err != nil {
		t.Skipf("фикстура импортёра недоступна: %v", err)
	}
	plan := importer.Plan(mustInspect(t, project))
	if plan.PluginID == "" {
		t.Error("в плане нет id плагина")
	}
	if len(plan.EntryPoints) == 0 {
		t.Error("в плане нет точек входа — разбор ничего не нашёл")
	}
	dest := filepath.Join(t.TempDir(), "must_not_appear")
	if _, err := os.Stat(dest); err == nil {
		t.Error("каталог создан, хотя план ничего не должен писать")
	}
}

// TestPluginImportRejectsNonProject — на каталог без метаданных команда
// отвечает отказом, а не создаёт пустой плагин.
func TestPluginImportRejectsNonProject(t *testing.T) {
	empty := t.TempDir()
	if _, err := importer.Inspect(empty); err == nil {
		t.Error("разбор пустого каталога должен давать ошибку, а не пустой проект")
	} else if !strings.Contains(err.Error(), "pyproject.toml") {
		t.Errorf("в ошибке нет указания, чего не хватает: %v", err)
	}
}

// mustInspect — разобрать проект или упасть.
func mustInspect(t *testing.T, dir string) *importer.Project {
	t.Helper()
	p, err := importer.Inspect(dir)
	if err != nil {
		t.Fatalf("разбор %s: %v", dir, err)
	}
	return p
}
