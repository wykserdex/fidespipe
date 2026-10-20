package verdoc

// Тесты проверки версий. Ключевой сценарий — расхождение: именно его эта
// проверка и поймала (VERSION 0.34.0 против 0.33.0 в README), поэтому тест без
// расхождения ничего не проверял бы.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFixture — синтетический репозиторий, повторяющий формы, в которых
// версия и минимум Go названы по-настоящему. Собирается на каждый тест, чтобы
// правки одного случая не ломали другой.
type repoFixture struct {
	dir     string
	version string
}

func newRepo(t *testing.T, version string) *repoFixture {
	t.Helper()
	dir := t.TempDir()
	r := &repoFixture{dir: dir, version: version}
	r.write(t, "VERSION", version+"\n")
	r.write(t, "go.mod", "module fidespipe\n\ngo 1.26\n")
	r.write(t, "README.md", strings.Join([]string{
		"# Fidespipe",
		"",
		"Требования: Go 1.26+ нужен только для сборки из исходников.",
		"",
		"## Что умеет",
		"",
		"| Возможность | Как выглядит |",
		"|---|---|",
		"| Агенты (MCP) | 8 инструментов: `list_plugins`, `describe_plugin`, `validate_pipeline`, `plan_pipeline`, `run_pipeline`, `get_run`, `cancel_run`, `exec_plugin` |",
		"",
		"## Релизы",
		"",
		"Тег релиза совпадает с [`VERSION`](VERSION) — сейчас `" + version + "`;",
		"protocol version — [`protocol/VERSION`](protocol/VERSION) (`0.2`).",
		"",
	}, "\n"))
	r.write(t, "README.en.md", strings.Join([]string{
		"# Fidespipe",
		"",
		"## Current contract",
		"",
		"- Product version: `" + version + "` from [`VERSION`](VERSION)",
		"- Pipeline/plugin protocol: `0.2` from [`protocol/VERSION`](protocol/VERSION)",
		"",
		"## Build and run",
		"",
		"Building from source needs Go 1.26 or newer.",
		"",
	}, "\n"))
	r.write(t, "CONTRIBUTING.md", strings.Join([]string{
		"# Contributing",
		"",
		"1. Use Go 1.26 or newer and Python 3 for plugin fixtures.",
		"",
	}, "\n"))
	r.write(t, "docs/versioning.md", strings.Join([]string{
		"# Версионирование Fidespipe",
		"",
		"| Ось | Источник | Текущее значение |",
		"|---|---|---|",
		"| Product / application | `VERSION` | `" + version + "` |",
		"| Pipeline and plugin protocol | `protocol/VERSION` | `0.2` |",
		"",
	}, "\n"))
	r.write(t, "docs/architecture.md", strings.Join([]string{
		"# Архитектура",
		"",
		"## Версии",
		"",
		"- `VERSION` (корень) — единственная версия приложения; текущий релиз —",
		"  `" + version + "`, а release tag обязан совпадать с `VERSION`.",
		"",
	}, "\n"))
	r.write(t, mcpServerFile, mcpSource)
	// schemas.go тоже часть фикстуры: проверка сверяет объявляемый список с
	// принимаемым, и без этого файла она честно сказала бы «проверка сломана».
	// Научить её молча пропускать отсутствующий файл нельзя: пропуск был бы
	// неотличим от успеха.
	r.write(t, mcpSchemasFile, mcpSchemasSource)
	return r
}

// mcpSchemasSource — объявления инструментов. Список обязан совпадать с
// dispatch-таблицей mcpSource, иначе фикстура сама нарушает то, что проверка
// ищет, и тесты перестают что-либо значить.
const mcpSchemasSource = "package mcp\n\n" +
	"type Tool struct{ Name, Description string }\n\n" +
	"func toolDefs() []Tool {\n\treturn []Tool{\n" +
	"\t\t{Name: \"list_plugins\", Description: \"d\"},\n" +
	"\t\t{Name: \"describe_plugin\", Description: \"d\"},\n" +
	"\t\t{Name: \"validate_pipeline\", Description: \"d\"},\n" +
	"\t\t{Name: \"plan_pipeline\", Description: \"d\"},\n" +
	"\t\t{Name: \"run_pipeline\", Description: \"d\"},\n" +
	"\t\t{Name: \"get_run\", Description: \"d\"},\n" +
	"\t\t{Name: \"cancel_run\", Description: \"d\"},\n" +
	"\t\t{Name: \"exec_plugin\", Description: \"d\"},\n" +
	"\t}\n}\n"

// mcpSource — настоящая форма callTool: восемь инструментов, восьмой за
// opt-in флагом. Ровно та же разметка, что в internal/mcp/server.go.
const mcpSource = `package mcp

type Server struct{ trust bool }

func (s *Server) callTool(name string) (string, bool) {
	switch name {
	case "list_plugins":
		return "", false
	case "describe_plugin":
		return "", false
	case "validate_pipeline":
		return "", false
	case "plan_pipeline":
		return "", false
	case "run_pipeline":
		return "", false
	case "get_run":
		return "", false
	case "cancel_run":
		return "", false
	case "exec_plugin":
		return "", false
	default:
		return "", true
	}
}
`

func (r *repoFixture) write(t *testing.T, rel, body string) {
	t.Helper()
	path := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *repoFixture) read(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (r *repoFixture) patch(t *testing.T, rel, old, new string) {
	t.Helper()
	body := r.read(t, rel)
	if !strings.Contains(body, old) {
		t.Fatalf("в %s нет фрагмента %q", rel, old)
	}
	r.write(t, rel, strings.Replace(body, old, new, 1))
}

// Главный сценарий: сходится.
func TestCheckRepoPassesOnConsistentRepo(t *testing.T) {
	r := newRepo(t, "0.34.0")
	out, err := CheckRepo(r.dir)
	if err != nil {
		t.Fatalf("согласованный репозиторий не прошёл: %v\n%s", err, out)
	}
	if !strings.Contains(out, "0.34.0") {
		t.Fatalf("в сводке нет версии: %q", out)
	}
}

// Реальный репозиторий обязан проходить: иначе проверка зелёная на синтетике
// и красная на том, ради чего написана.
func TestCheckRepoPassesOnRealRepository(t *testing.T) {
	repo, err := findRepoRoot()
	if err != nil {
		t.Skipf("репозиторий не найден: %v", err)
	}
	out, err := CheckRepo(repo)
	if err != nil {
		t.Fatalf("настоящий репозиторий не прошёл проверку версий: %v\n%s", err, out)
	}
}

// Инструмент вызывается, но не объявлен. Именно так был сломан exec_plugin:
// он вызывался диспетчером, значился в README, и сверка README↔callTool была
// зелёной — а объявления в tools/list у него не было, то есть агент не мог его
// открыть через discovery. Проверка обязана ловить это сама.
func TestCheckRepoFailsOnUnpublishedTool(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.patch(t, mcpSchemasFile, "\t\t{Name: \"exec_plugin\", Description: \"d\"},\n", "")

	out, err := CheckRepo(r.dir)
	if err == nil {
		t.Fatalf("вызываемый, но не объявленный инструмент должен ронять проверку:\n%s", out)
	}
	if !strings.Contains(out, "discovery") {
		t.Fatalf("ошибка должна называть discovery, а не просто констатировать расхождение:\n%s", out)
	}
}

// Обратное расхождение опаснее: tools/list обещает инструмент, которого сервер
// не принимает. Агент строит план на шаге, которого не существует.
func TestCheckRepoFailsOnPhantomTool(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.patch(t, mcpSchemasFile, "\t\t{Name: \"exec_plugin\", Description: \"d\"},\n",
		"\t\t{Name: \"exec_plugin\", Description: \"d\"},\n\t\t{Name: \"ghost_tool\", Description: \"d\"},\n")

	out, err := CheckRepo(r.dir)
	if err == nil {
		t.Fatalf("объявленный, но не принимаемый инструмент должен ронять проверку:\n%s", out)
	}
	if !strings.Contains(out, "ghost_tool") {
		t.Fatalf("ошибка должна называть инструмент-фантом:\n%s", out)
	}
}

// Проверка не должна выродиться в «всё зелёное» на пустом разборе: если
// разобрать объявления не удалось, это поломка проверки, а не успех.
func TestCheckRepoFailsWhenToolDefsUnparseable(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.write(t, mcpSchemasFile, "package mcp\n\nfunc toolDefs() []Tool { return nil }\n")

	out, err := CheckRepo(r.dir)
	if err == nil {
		t.Fatalf("пустой разбор объявлений должен ронять проверку, а не проходить:\n%s", out)
	}
	// Сообщение приходит в err, а не в отчёт: этот случай возвращается раньше,
	// чем набирается сводка, и подменять его расхождением нельзя — иначе
	// поломка проверки выглядела бы как «нашлось расхождение».
	if !strings.Contains(err.Error(), "сломана") {
		t.Fatalf("ошибка должна отличать поломку проверки от расхождения: %v", err)
	}
}

// Расхождение, которое проверка и ловила: VERSION 0.34.0, README 0.33.0.
func TestCheckRepoFailsOnReadmeVersionDrift(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.patch(t, "README.md", "сейчас `0.34.0`", "сейчас `0.33.0`")
	r.patch(t, "README.en.md", "Product version: `0.34.0`", "Product version: `0.33.0`")

	out, err := CheckRepo(r.dir)
	if err == nil {
		t.Fatalf("расхождение версий не поймано:\n%s", out)
	}
	if !strings.Contains(out, "0.33.0") || !strings.Contains(out, "0.34.0") {
		t.Fatalf("в отчёте нет ни одной из версий:\n%s", out)
	}
	for _, rel := range []string{"README.md", "README.en.md"} {
		if !strings.Contains(out, rel) {
			t.Fatalf("%s не назван в отчёте:\n%s", rel, out)
		}
	}
}

// Расхождение в docs/ — того же класса, что и в README.
func TestCheckRepoFailsOnDocsVersionDrift(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.patch(t, "docs/versioning.md", "| `VERSION` | `0.34.0` |", "| `VERSION` | `0.33.0` |")
	r.patch(t, "docs/architecture.md", "  `0.34.0`,", "  `0.33.0`,")

	out, err := CheckRepo(r.dir)
	if err == nil {
		t.Fatalf("расхождение в docs не поймано:\n%s", out)
	}
	if !strings.Contains(out, "docs/versioning.md") || !strings.Contains(out, "docs/architecture.md") {
		t.Fatalf("оба документа должны быть названы:\n%s", out)
	}
}

// Примеры предыдущих релизов в том же блоке — не расхождение:
// блоке про VERSION обязаны оставаться в покое.
func TestCheckRepoIgnoresNumberingExamples(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.patch(t, "docs/versioning.md", "| `VERSION` | `0.34.0` |",
		"| `VERSION` | `0.34.0` | предыдущие релизы `0.32.0`, `0.33.0`; тег `v0.33.0` |")
	if out, err := CheckRepo(r.dir); err != nil {
		t.Fatalf("примеры нумерации приняты за расхождение: %v\n%s", err, out)
	}
}

// Минимум Go: README обещает 1.22, go.mod требует 1.26.
func TestCheckRepoFailsOnGoFloorDrift(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.patch(t, "README.md", "Go 1.26+", "Go 1.22+")
	r.patch(t, "README.en.md", "Go 1.26 or newer", "Go 1.22 or newer")
	r.patch(t, "CONTRIBUTING.md", "Go 1.26 or newer", "Go 1.22 or newer")

	out, err := CheckRepo(r.dir)
	if err == nil {
		t.Fatalf("расхождение минимума Go не поймано:\n%s", out)
	}
	if !strings.Contains(out, "go.mod требует go 1.26") {
		t.Fatalf("в отчёте нет обещания go.mod:\n%s", out)
	}
}

// Отсутствие директивы `go` — поломка проверки, а не успех: иначе «зелёный CI"
// означал бы «проверка не нашла чего сравнивать».
func TestCheckRepoFailsWhenGoDirectiveMissing(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.write(t, "go.mod", "module fidespipe\n")
	out, err := CheckRepo(r.dir)
	if err == nil || !strings.Contains(err.Error(), "директива `go X.Y`") {
		t.Fatalf("пустой go.mod обязан ломать проверку, а не проходить её: %v\n%s", err, out)
	}
}

// Число инструментов MCP в README против dispatch-таблицы callTool.
func TestCheckRepoFailsOnMCPToolCountDrift(t *testing.T) {
	r := newRepo(t, "0.34.0")
	// Та самая ошибка, что была в репозитории: восьмой инструмент принимается,
	// но не назван.
	r.patch(t, "README.md", ", `cancel_run`, `exec_plugin` |", ", `cancel_run` |")

	out, err := CheckRepo(r.dir)
	if err == nil {
		t.Fatalf("не названный инструмент не пойман:\n%s", out)
	}
	if !strings.Contains(out, "exec_plugin") {
		t.Fatalf("в отчёте нет имени инструмента:\n%s", out)
	}
}

// Лишний инструмент в README — тоже расхождение: README обещает то, чего
// вызов не принимает.
func TestCheckRepoFailsOnUnknownReadmeTool(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.patch(t, "README.md", "`cancel_run`", "`cancel_run`, `teleport_run`")

	out, err := CheckRepo(r.dir)
	if err == nil {
		t.Fatalf("инструмент, которого нет в callTool, не пойман:\n%s", out)
	}
	if !strings.Contains(out, "teleport_run") {
		t.Fatalf("в отчёте нет имени инструмента:\n%s", out)
	}
}

// Строка таблицы MCP исчезла — проверка обязана это заметить, а не молча
// признать, что сверять нечего.
func TestCheckRepoFailsWhenMCPRowRemoved(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.patch(t, "README.md", "| Агенты (MCP) | 8 инструментов: `list_plugins`, `describe_plugin`, `validate_pipeline`, `plan_pipeline`, `run_pipeline`, `get_run`, `cancel_run`, `exec_plugin` |\n", "")

	out, err := CheckRepo(r.dir)
	if err == nil || !strings.Contains(err.Error(), "сверять нечего") {
		t.Fatalf("удалённая строка таблицы обязана ломать проверку: %v\n%s", err, out)
	}
}

// callTool исчез или потерял switch — проверка не должна превращаться в
// вакуумно-зелёную.
func TestCheckRepoFailsWhenDispatchTableEmpty(t *testing.T) {
	r := newRepo(t, "0.34.0")
	r.write(t, mcpServerFile, "package mcp\n\ntype Server struct{}\n")

	out, err := CheckRepo(r.dir)
	if err == nil || !strings.Contains(err.Error(), "проверка сломана") {
		t.Fatalf("пустая dispatch-таблица обязана ломать проверку: %v\n%s", err, out)
	}
}

// findRepoRoot — корень репозитория по go.mod вверх от рабочего каталога.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// Буквенная схема отвергается. Она выглядела совместимой — свой же код её и
// породил, — но тег `v0.33c` для Go не версия модуля, поэтому `go install`
// молча не работал. Инвариант теперь проверяется, а не описывается в прозе.
func TestCheckRepoRejectsLetteredProductVersion(t *testing.T) {
	r := newRepo(t, "0.33c")
	out, err := CheckRepo(r.dir)
	if err == nil {
		t.Fatalf("буквенная версия прошла проверку:\n%s", out)
	}
	if !strings.Contains(err.Error(), "SemVer") {
		t.Fatalf("отказ обязан объяснять причину (SemVer), а не просто констатировать: %v", err)
	}
}
