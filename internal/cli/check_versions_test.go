package cli

// Шаг `versions` в `fidespipe check`: дрейф версий возвращается, если за него
// никто не спрашивает. Проверка обязана быть в наборе шагов, быть быстрой,
// быть видна в --help и падать на расхождении.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stepByName(t *testing.T, name string) checkStep {
	t.Helper()
	for _, s := range checkSteps() {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("в fidespipe check нет шага %q — дрейф версий снова никем не проверяется", name)
	return checkStep{}
}

func TestVersionsStepExistsAndIsFast(t *testing.T) {
	step := stepByName(t, "versions")
	if !step.fast {
		t.Error("шаг versions помечен медленным: проверка версий обязана идти в --fast")
	}
	// --only отбирает шаги по имени, --fast — по флагу. Обе выборки не
	// должны терять шаг, иначе он есть, а добраться до него нельзя.
	var byOnly, byFast int
	for _, s := range checkSteps() {
		if s.name == "versions" {
			byOnly++
		}
		if s.fast && s.name == "versions" {
			byFast++
		}
	}
	if byOnly != 1 || byFast != 1 {
		t.Fatalf("шаг versions выбирается --only=%d, --fast=%d раз (ждали по одному)", byOnly, byFast)
	}
}

func TestVersionsStepListedInHelp(t *testing.T) {
	help := captureStderr(t, printCheckHelp)
	if !strings.Contains(help, "versions") {
		t.Fatalf("шаг versions не назван в --help:\n%s", help)
	}
	// Список в --help написан руками, а checkSteps — нет. Расхождение между
	// ними означает то же самое, ради чего добавлен шаг: человек узнаёт о
	// проверке из документации, которой уже нет.
	if !strings.Contains(help, "--fast") || !strings.Contains(help, "versions") {
		t.Fatalf("список быстрых шагов в --help не содержит versions:\n%s", help)
	}
}

// repoUnderTest — копия файлов, которые читает проверка версий. Реальный
// репозиторий не трогается: тест подменяет VERSION, и падение на середине
// не должно оставлять после себя «9.99z» в рабочем дереве.
func repoUnderTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	src := findRepoRoot(wd)
	if src == "" {
		t.Skipf("репозиторий не найден от %s", wd)
	}
	dst := t.TempDir()
	for _, rel := range []string{
		"VERSION", "go.mod", "README.md", "README.en.md", "CONTRIBUTING.md",
		"docs/versioning.md", "docs/architecture.md",
		"internal/mcp/server.go", "internal/mcp/schemas.go",
	} {
		body, readErr := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if readErr != nil {
			t.Fatal(readErr)
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			t.Fatal(mkErr)
		}
		if writeErr := os.WriteFile(target, body, 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	return dst
}

// Согласованная копия проходит; расхождение — нет. Оба утверждения нужны:
// одна зелёная половина означала бы либо сломанную проверку, либо тест,
// который ничего не проверяет.
func TestVersionsStepPassesAndFailsOnDrift(t *testing.T) {
	repo := repoUnderTest(t)
	step := stepByName(t, "versions")

	if out, err := step.run(checkOpts{repo: repo}); err != nil {
		t.Fatalf("копия репозитория обязана проходить: %v\n%s", err, out)
	}

	// Рассинхрон версии и документов — тот случай, ради которого шаг и написан.
	// Версия валидная по схеме: иначе сработала бы проверка SemVer ниже, и тест
	// проверял бы не то, что заявляет.
	versionPath := filepath.Join(repo, "VERSION")
	if err := os.WriteFile(versionPath, []byte("9.99.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := step.run(checkOpts{repo: repo})
	if err == nil {
		t.Fatalf("расхождение версий не поймано:\n%s", out)
	}
	if !strings.Contains(out, "9.99.9") {
		t.Fatalf("в отчёте нет подставленной версии:\n%s", out)
	}
	for _, rel := range []string{"README.md", "README.en.md"} {
		if !strings.Contains(out, rel) {
			t.Fatalf("%s не назван в отчёте:\n%s", rel, out)
		}
	}
}

// Буквенная схема отклоняется на уровне шага: VERSION без SemVer означает тег,
// которого не существует для go install, — и это не «расхождение документов»,
// а отказ ещё до сверки.
func TestVersionsStepRejectsLetteredVersion(t *testing.T) {
	repo := repoUnderTest(t)
	step := stepByName(t, "versions")

	versionPath := filepath.Join(repo, "VERSION")
	if err := os.WriteFile(versionPath, []byte("9.99z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := step.run(checkOpts{repo: repo})
	if err == nil {
		t.Fatalf("буквенная версия прошла проверку:\n%s", out)
	}
	if !strings.Contains(err.Error(), "SemVer") {
		t.Fatalf("отказ обязан объяснять причину (SemVer): %v", err)
	}
}

// captureStderr — помощник: печать в os.Stderr перехватывается на время вызова.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		body, _ := io.ReadAll(r)
		done <- string(body)
	}()
	fn()
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}
