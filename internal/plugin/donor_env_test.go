package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/registry"
)

// TestDonorEnvRejectsUnsafeName — имя переменной проверяется.
//
// Подмена переменной окружения через имя — уязвимость: имя приходит из
// манифеста плагина, а манифест может быть чужим. Поэтому берётся только
// имя в верхнем регистре из букв, цифр и подчёркивания.
func TestDonorEnvRejectsUnsafeName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", "BAD NAME", "PATH", "lower_case", "1BAD", "X=Y"} {
		m := &pipeline.Manifest{
			Dir:     dir,
			Runtime: pipeline.Runtime{Donor: &pipeline.Donor{BinEnv: name}},
		}
		if v := donorEnvFor(m); v != "" {
			t.Errorf("имя %q принято, переменная %q", name, v)
		}
	}
}

// TestDonorEnvFromLock — путь к донору берётся из метки установки: он
// зависит от корня установки, а тот у ядра другой.
func TestDonorEnvFromLock(t *testing.T) {
	dir := t.TempDir()
	lock := registry.Lock{Name: "x", Source: "s", Path: "p", Version: "v"}
	lock.DonorPath = filepath.Join(dir, "tool")
	if err := registry.WriteLock(dir, lock); err != nil {
		t.Fatal(err)
	}
	m := &pipeline.Manifest{
		Dir:     dir,
		Runtime: pipeline.Runtime{Donor: &pipeline.Donor{BinEnv: "TOOL_BIN"}},
	}
	got := donorEnvFor(m)
	want := "TOOL_BIN=" + lock.DonorPath
	if got != want {
		t.Errorf("переменная = %q, ожидалась %q", got, want)
	}
}

// TestDonorEnvAbsentWithoutLock — без метки установки переменной нет: путь в
// никуда не ведёт, и «кажется, работает» хуже, чем «не настроено».
func TestDonorEnvAbsentWithoutLock(t *testing.T) {
	dir := t.TempDir()
	if _, err := os.Stat(filepath.Join(dir, registry.LockFile)); err == nil {
		t.Fatal("метка не должна существовать")
	}
	m := &pipeline.Manifest{
		Dir:     dir,
		Runtime: pipeline.Runtime{Donor: &pipeline.Donor{BinEnv: "TOOL_BIN"}},
	}
	if v := donorEnvFor(m); v != "" {
		t.Errorf("без метки установки переменная не должна появляться: %q", v)
	}
}
