package buildinfo

import (
	"os"
	"path/filepath"
	"testing"
)

// Регресс, из-за которого пакет появился: релизная сборка показывает версию
// из чужого файла VERSION в текущем каталоге. Инжект обязан побеждать файл.
func TestInjectedVersionBeatsWorkingDirFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("3.0.0-alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	defer func(old string) { Version = old }(Version)
	Version = "0.34.0"

	if got := Resolve(); got != "0.34.0" {
		t.Fatalf("Resolve() = %q, хочу инжектированную 0.34.0 (файл в CWD не должен побеждать)", got)
	}
}

// Dev-сборка без инжекта обязана по-прежнему видеть VERSION из рабочего
// каталога: иначе `go run ./cmd/fidespipe check` в чекауте репо перестанет
// печатать версию.
func TestDevBuildFallsBackToWorkingDirFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	defer func(old string) { Version = old }(Version)
	Version = ""

	// Файл рядом с тестовым бинарником (executable tests) приоритетнее CWD,
	// поэтому проверяем именно «дошло до CWD», а не конкретный источник.
	if got := Resolve(); got != "9.9.9" {
		t.Fatalf("Resolve() = %q, хочу 9.9.9 из файла VERSION", got)
	}
}

func TestGarbageVersionFileIsIgnored(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"мусор вместо версии", "это не версия, а текст с пробелами\n"},
		{"пустой файл", "\n\n"},
		{"слишком длинное", string(make([]byte, maxVersionLen+1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)

			defer func(old string) { Version = old }(Version)
			Version = ""

			if got := Resolve(); got != devVersion {
				t.Fatalf("Resolve() = %q, хочу %q: содержимое файла не похоже на версию", got, devVersion)
			}
		})
	}
}

// Без инжекта, без файлов — ровно "dev", а не пустая строка: пустая версия в
// serverInfo/--help выглядит как сломанная сборка.
func TestNoSourcesMeansDev(t *testing.T) {
	dir := t.TempDir() // без VERSION
	t.Chdir(dir)

	defer func(old string) { Version = old }(Version)
	Version = ""

	if got := Resolve(); got != devVersion {
		t.Fatalf("Resolve() = %q, хочу %q", got, devVersion)
	}
}
