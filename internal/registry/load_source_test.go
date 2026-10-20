package registry

// F-05: источник реестра — аргумент git, поэтому проверяется ДО exec.
// `--upload-pack=…` в Load(source) иначе становится опцией git и выполняет
// произвольную команду. Разделитель `--` — вторая линия обороны.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileRepoURL — file://-URL, который переживает и Windows (C:\… → file:///C:/…).
func fileRepoURL(dir string) string {
	if vol := filepath.VolumeName(dir); vol != "" {
		return "file:///" + filepath.ToSlash(vol) + "/" + filepath.ToSlash(strings.TrimPrefix(dir, vol))
	}
	return "file://" + filepath.ToSlash(dir)
}

// Регресс: ref, начинающийся с "-", обязан быть отвергнут ДО запуска git.
// PATH обнулён — если бы дело дошло до exec, ошибка была бы «git не найден».
func TestLoadRejectsOptionLikeRegistrySource(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, source := range []string{
		"--upload-pack=touch /tmp/fidespipe-pwned",
		"--config=core.sshCommand=id",
		"-c",
		"-u/tmp/fidespipe-pwned",
		"--upload-pack=calc.exe",
	} {
		_, err := Load(source)
		if err == nil {
			t.Fatalf("option-like source %q was accepted", source)
		}
		if !strings.Contains(err.Error(), "небезопасный source") {
			t.Fatalf("source %q: exec был вызван до валидации: %v", source, err)
		}
	}
}

// Валидные источники (в т.ч. Windows-пути и URL) валидатор пропускает —
// иначе валидация сломала бы локальные/сетевые реестры.
func TestValidateSourceAcceptsLocalAndURLSources(t *testing.T) {
	for _, source := range []string{
		"https://example.com/repo.git",
		"ssh://git@example.com/repo.git",
		"git://example.com/repo.git",
		"file:///srv/registry.git",
		"registry.yaml",
		"registry-addons.yaml",
		"../registry",
		`C:\repos\registry`,
		"C:/repos/registry",
	} {
		if err := ValidateSource(source); err != nil {
			t.Fatalf("valid source %q rejected: %v", source, err)
		}
	}
	// Пробел в ref отвергается (в URL он и должен быть %20), но локальный
	// путь с пробелами обязан грузиться — валидация стоит только на clone-ветке.
	for _, source := range []string{
		"-",
		"--upload-pack=id",
		"--config=core.sshCommand=id",
		"-x",
		"repo with space",
		"repo\twith\ttab",
		"repo::evil",
		"ftp://example.com/repo.git",
	} {
		if err := ValidateSource(source); err == nil {
			t.Fatalf("unsafe source accepted: %q", source)
		}
	}
}

func TestLoadAcceptsLocalPathWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Program Files", "fidespipe registry")
	writeFile(t, filepath.Join(dir, RegistryFile), `
version: "0.1"
presets:
  demo:
    source: https://example.com/demo
    path: demo.yaml
`)
	h, err := Load(dir)
	if err != nil {
		t.Fatalf("локальный каталог с пробелами отвергнут: %v", err)
	}
	defer h.Close()
	if _, ok := h.GetPreset("demo"); !ok {
		t.Fatal("preset demo не найден")
	}
	file := filepath.Join(dir, RegistryFile)
	if _, err := Load(file); err != nil {
		t.Fatalf("локальный файл с пробелами в пути отвергнут: %v", err)
	}
}

// Реальный клон по file://: валидация + разделитель `--` не ломают
// ни git-URL источников, ни Windows-пути.
func TestLoadClonesURLSourceAfterValidation(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "load@test")
	git(t, repo, "config", "user.name", "load")
	writeFile(t, filepath.Join(repo, RegistryFile), `
version: "0.1"
presets:
  demo:
    source: https://example.com/demo
    path: pipelines/demo.yaml
    commit: 0123456789abcdef0123456789abcdef01234567
`)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "registry")

	h, err := Load(fileRepoURL(repo))
	if err != nil {
		t.Fatalf("URL-источник отвергнут: %v", err)
	}
	defer h.Close()
	if h.Registry.Version != FormatVersion {
		t.Fatalf("version: %q", h.Registry.Version)
	}
	e, ok := h.GetPreset("demo")
	if !ok || e.Path != "pipelines/demo.yaml" {
		t.Fatalf("preset demo: %+v ok=%v", e, ok)
	}
	if h.tmp == "" {
		t.Fatal("клон должен быть временным каталогом")
	}
	if _, err := os.Stat(h.tmp); err != nil {
		t.Fatalf("временный клон пропал: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(h.tmp); !os.IsNotExist(err) {
		t.Fatalf("временный клон не убран: %v", err)
	}
}
