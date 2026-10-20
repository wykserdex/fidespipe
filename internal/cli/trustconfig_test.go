package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wykserdex/fidespipe/internal/plugin"
)

// Конфиг доверия не должен молча приходить из рабочего каталога: этот каталог
// задаёт не оператор, а тот, кто положил сюда пайплайн. Чужой клон репозитория
// с приложенным fidespipe-trust.yaml проходит проверки N3 (владелец — пользователь,
// права 0644) и расширяет allow-list, то есть выдаёт доверие оператора.
func TestTrustConfigPathIgnoresWorkingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, plugin.TrustConfigFile),
		[]byte("version: \"0.1\"\ntrusted_plugins: []\n"), 0o644); err != nil {
		t.Fatalf("write trust config: %v", err)
	}
	t.Chdir(dir)

	got := trustConfigPath("")
	if filepath.Base(got) == plugin.TrustConfigFile && !filepath.IsAbs(got) {
		t.Fatalf("путь конфига взят из рабочего каталога: %q", got)
	}
	abs, err := filepath.Abs(got)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if abs == filepath.Join(dir, plugin.TrustConfigFile) {
		t.Fatalf("конфиг из рабочего каталога не должен использоваться: %q", got)
	}
}

// Явный путь оператора сильнее всего: и флага, и переменной окружения.
func TestTrustConfigPathOverrideWins(t *testing.T) {
	t.Setenv("FIDESPIPE_TRUST_CONFIG", filepath.Join(t.TempDir(), "from-env.yaml"))
	if got := trustConfigPath("from-flag.yaml"); got != "from-flag.yaml" {
		t.Fatalf("trustConfigPath(override) = %q", got)
	}
}

// Переменная окружения нужна там, где флаг передать некуда: CI, вызов из
// другого языка, обёртка поверх бинаря.
func TestTrustConfigPathFromEnv(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "from-env.yaml")
	t.Setenv("FIDESPIPE_TRUST_CONFIG", envPath)
	if got := trustConfigPath(""); got != envPath {
		t.Fatalf("trustConfigPath(\"\") = %q, ожидался %q из FIDESPIPE_TRUST_CONFIG", got, envPath)
	}
}

// Пустой путь = «конфига нет», а не путь в текущий каталог. Иначе
// LoadTrustConfig снова уехал бы в чужой cwd.
func TestTrustConfigPathIsAbsoluteOrEmpty(t *testing.T) {
	t.Setenv("FIDESPIPE_TRUST_CONFIG", "")
	t.Chdir(t.TempDir())
	got := trustConfigPath("")
	if got == "" {
		return
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("путь конфига относительный: %q", got)
	}
}

// LoadTrustConfig на пустом пути обязан дать пустой allow-list без ошибки:
// это «оператор ничего не настроил», а не сбой.
func TestLoadTrustConfigEmptyPath(t *testing.T) {
	list, err := plugin.LoadTrustConfig("")
	if err != nil {
		t.Fatalf("LoadTrustConfig(\"\"): %v", err)
	}
	if list == nil {
		t.Fatal("allow-list = nil")
	}
	if list.Allows("any", "deadbeef") {
		t.Fatal("пустой allow-list не должен ничего разрешать")
	}
}
