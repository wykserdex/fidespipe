// Package verdoc сверяет версию продукта с тем, что о ней написано в
// документации, и заявленный минимум Go с директивой go в go.mod.
//
// Зачем. Расхождение класса «документ обещает одно, репозиторий другое»
// находится только глазами, и в этом проекте оно уже было: VERSION держал
// `0.33a`, а README.md и README.en.md — `0.32c`, то есть релиз по тегу
// читался как другая версия. Проверка в CI этого не ловила: там сверяются
// VERSION с CHANGELOG, а README не участвует.
//
// Вторая половина той же болезни: README обещал «Go 1.22+», тогда как
// директива `go` в go.mod стояла на EOL-версии 1.22, а CI собирал 1.26.x.
// Требование из go.mod ниже фактически тестируемого — это заявленный минимум,
// который не проверяет ни один прогон.
//
// Четыре группы проверок:
//
//	(1) VERSION против README.md и README.en.md;
//	(2) VERSION против docs/versioning.md и docs/architecture.md;
//	(3) директива `go` в go.mod против минимума, названного в README.md,
//	    README.en.md и CONTRIBUTING.md;
//	(4) список MCP-инструментов в README.md против таблицы вызовов callTool
//	    в internal/mcp/server.go.
//
// Про (4): README называл «7 инструментов», а dispatch отдавал восемь —
// восьмой, exec_plugin, добавили вместе с политикой доверия, а таблицу в
// README и счётчик в комментарии к серверу обновили через одно место. Две
// цифры в одном документе, разъехавшиеся на единицу, — это ровно тот класс
// дрейфа, который проверка и ловит.
//
// Никаких зависимостей: go/ast, go/parser, os, path/filepath, regexp, strings.
package verdoc

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// versionLiteral — формат product-версии: `X.Y` плюс необязательная
	// буква. Именно он, а не произвольный SemVer: версия продукта по
	// docs/versioning.md НЕ является SemVer (там же про 0.32a).
	// versionLiteral — версия, названная в документе в бэктиках. Двухкомпонентный
	// вид оставлен ради protocol/VERSION (`0.2`) и исторических записей; три
	// компонента — product version по SemVer.
	versionLiteral = regexp.MustCompile("`([0-9]+\\.[0-9]+(?:\\.[0-9]+)?(?:-[0-9A-Za-z.-]+)?)`")

	// goDirectiveRe — директива `go` в go.mod. Якорь на начало строки:
	// `toolchain go1.27.0` не должен подменять собой `go 1.26`.
	goDirectiveRe = regexp.MustCompile(`(?m)^go[ \t]+([0-9]+\.[0-9]+)[ \t]*$`)

	// claimedGoRe — обещание минимума: `Go 1.26+` или `Go 1.26 or newer`.
	// Две формы — русская и английская вёрстка README, третья формулировка
	// CONTRIBUTING.
	claimedGoRe = regexp.MustCompile(`Go\s+([0-9]+\.[0-9]+)\s*(?:\+|or newer)`)

	// backtickedRe — слова в бэктиках: имена инструментов вида `run_pipeline`.
	backtickedRe = regexp.MustCompile("`([a-z][a-z0-9_]*)`")
)

var (
	// versionDocs — документы, где продуктовая версия названа явно. Документ,
	// который её не называет, пропускается: заставлять упоминать версию
	// половину документации незачем.
	versionDocs = []string{
		"README.md",
		"README.en.md",
		"docs/versioning.md",
		"docs/architecture.md",
	}

	// goDocs — файлы, где назван минимум Go для сборки из исходников.
	goDocs = []string{
		"README.md",
		"README.en.md",
		"CONTRIBUTING.md",
	}
)

const (
	mcpServerFile  = "internal/mcp/server.go"
	mcpSchemasFile = "internal/mcp/schemas.go"
	mcpToolDefs    = "toolDefs"
	mcpReadmeRow   = "| Агенты (MCP) |"
	mcpCallTool    = "callTool"
)

// CheckRepo сверяет версии. Отчёт возвращается строкой, а решение — через
// error: так шаг fidespipe check покажет и короткую сводку, и подробности.
func CheckRepo(repo string) (string, error) {
	current, err := readVersionFile(filepath.Join(repo, "VERSION"))
	if err != nil {
		return "", err
	}

	var problems, notes []string

	// (1)+(2) версия продукта против документов.
	checked := 0
	for _, rel := range versionDocs {
		raw, found, err := readDoc(repo, rel)
		if err != nil {
			return "", err
		}
		if !found {
			notes = append(notes, fmt.Sprintf("%s: версия не названа в проверяемом виде — нечего сверять", rel))
			continue
		}
		checked++
		problems = append(problems, checkVersionDoc(rel, raw, current)...)
	}
	// Страховка от вакуумно-зелёной проверки: если не нашлось НИ ОДНОГО
	// документа с версией, зелёный CI означал бы «проверка ничего не нашла».
	if checked == 0 {
		return "", fmt.Errorf("ни один из %s не называет версию в проверяемом виде — проверка сломана, а не прошла",
			strings.Join(versionDocs, ", "))
	}

	// (3) минимум Go: директива go.mod против того, что написано в документации.
	goProblems, goNotes, err := checkGoFloor(repo)
	if err != nil {
		return "", err
	}
	problems = append(problems, goProblems...)
	notes = append(notes, goNotes...)

	// (4) список MCP-инструментов.
	tools, err := dispatchedTools(filepath.Join(repo, filepath.FromSlash(mcpServerFile)))
	if err != nil {
		return "", err
	}
	if len(tools) == 0 {
		return "", fmt.Errorf("%s: в %s не найдено ни одного инструмента — проверка сломана, а не прошла",
			mcpServerFile, mcpCallTool)
	}
	readme, _, err := readDoc(repo, "README.md")
	if err != nil {
		return "", err
	}
	problems = append(problems, checkReadmeTools(readme, tools)...)
	notes = append(notes, fmt.Sprintf("MCP: %d инструментов принимает %s (%s)", len(tools), mcpCallTool, mcpServerFile))

	// (5) Публикуемый список против принимаемого.
	//
	// Сверки README против callTool недостаточно: она осталась бы зелёной, если
	// бы инструмент вызывался, но не публиковался в tools/list. Агент в таком
	// случае не может открыть инструмент через discovery и получает отказ,
	// ничего не объясняющий. Именно так и было с exec_plugin: он вызывался,
	// в README значился и проверку проходил — а объявления у него не было.
	published, err := publishedTools(filepath.Join(repo, filepath.FromSlash(mcpSchemasFile)))
	if err != nil {
		return "", err
	}
	if len(published) == 0 {
		return "", fmt.Errorf("%s: в %s не найдено ни одного инструмента — проверка сломана, а не прошла",
			mcpSchemasFile, mcpToolDefs)
	}
	problems = append(problems, checkPublishedVsDispatched(published, tools)...)
	notes = append(notes, fmt.Sprintf("MCP: %d инструментов публикует %s (%s)", len(published), mcpToolDefs, mcpSchemasFile))

	notes = append([]string{fmt.Sprintf("VERSION %s сходится с %s", current, strings.Join(versionDocs, ", "))}, notes...)
	if len(problems) > 0 {
		return strings.Join(append([]string{"расхождения:"}, problems...), "\n"),
			fmt.Errorf("версии в документации разошлись с репозиторием (%d):\n%s",
				len(problems), strings.Join(problems, "\n"))
	}
	return strings.Join(notes, "\n"), nil
}

// semverPattern — версия продукта. Схема закреплена здесь, а не в документации,
// потому что цена нарушения — не косметика: Go принимает как версию модуля
// только SemVer, поэтому тег вида `v0.33c` не существует для `go install`, а
// `VERSION` — то, с чем тег обязан совпадать. Пока проверка жила только в
// прозе, буквенная схема тихо держала установку закрытой.
var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

func readVersionFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", fmt.Errorf("VERSION пуст")
	}
	if !semverPattern.MatchString(v) {
		return "", fmt.Errorf("VERSION %q не SemVer: тег с таким номером не существует для "+
			"go install (Go принимает только SemVer), а совпадение VERSION и тега — правило релиза. "+
			"Нужен вид X.Y.Z (например, 0.34.0), а не 0.33c", v)
	}
	return v, nil
}

// readDoc — текст документа с нормализованными переводами строк.
// found=false означает «файла нет», а не «в нём ничего не нашли».
func readDoc(repo, rel string) (text string, found bool, err error) {
	raw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n"), true, nil
}

// checkVersionDoc сверяет один документ.
//
// Правило разбора: текст режется на блоки (абзацы, таблицы), и в первом блоке,
// где встречается слово VERSION, берётся ПЕРВАЯ версия в бэктиках. Это не
// «все числа должны совпадать»: в том же блоке docs/versioning.md лежат
// правила нумерации (`0.32a`, `0.32b`, `v0.32a`) и примеры, и требовать от
// них равенства текущей версии бессмысленно. Первая — та, о которой блок
// говорит «сейчас»; остальные — примеры рядом с правилом.
func checkVersionDoc(rel, text, current string) []string {
	for _, block := range blocksWithVersion(text) {
		m := versionLiteral.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		if m[1] == current {
			return nil
		}
		return []string{fmt.Sprintf("    %-22s называет версию %q, а VERSION = %q", rel, m[1], current)}
	}
	return nil
}

// blocksWithVersion — блоки текста, содержащие слово VERSION.
func blocksWithVersion(text string) []string {
	var blocks []string
	for _, block := range strings.Split(text, "\n\n") {
		if strings.Contains(block, "VERSION") {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// checkGoFloor сверяет директиву `go` в go.mod с минимумом, названным в
// документации. Расхождение опаснее неправды в README: инструментарий старее
// директивы просто отказывается собирать модуль, то есть «поднятое»
// требование превращается в отказ у того, кто честно поставил Go по README.
func checkGoFloor(repo string) (problems, notes []string, err error) {
	floor, err := readGoDirective(filepath.Join(repo, "go.mod"))
	if err != nil {
		return nil, nil, err
	}
	matched := 0
	for _, rel := range goDocs {
		text, found, err := readDoc(repo, rel)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			continue
		}
		m := claimedGoRe.FindStringSubmatch(text)
		if m == nil {
			notes = append(notes, fmt.Sprintf("%s: минимум Go не назван — нечего сверять", rel))
			continue
		}
		matched++
		if m[1] != floor {
			problems = append(problems, fmt.Sprintf("    %-22s обещает Go %s+, а go.mod требует go %s", rel, m[1], floor))
		}
	}
	if matched == 0 {
		return nil, nil, fmt.Errorf("ни один из %s не называет минимум Go — проверка сломана, а не прошла",
			strings.Join(goDocs, ", "))
	}
	notes = append(notes, fmt.Sprintf("минимум Go %s (директива go.mod) сходится с %s", floor, strings.Join(goDocs, ", ")))
	return problems, notes, nil
}

func readGoDirective(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := goDirectiveRe.FindSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("go.mod: не найдена директива `go X.Y` — проверка сломана, а не прошла")
	}
	return string(m[1]), nil
}

// dispatchedTools — имена, которые реально принимает callTool. Именно они и
// есть контракт для агента: tools/list — объявление, а вызов принимает то,
// что перечислено здесь.
func dispatchedTools(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name == nil || fn.Name.Name != mcpCallTool || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			var clauses []*ast.CaseClause
			switch node := n.(type) {
			case *ast.SwitchStmt:
				for _, stmt := range node.Body.List {
					if clause, ok := stmt.(*ast.CaseClause); ok {
						clauses = append(clauses, clause)
					}
				}
			case *ast.TypeSwitchStmt:
				// Инструменты не выбираются по типу, но такой switch придётся
				// разобрать тоже: иначе он молча выпал бы из подсчёта, и
				// проверка стала бы неполной без единого предупреждения.
				for _, stmt := range node.Body.List {
					if clause, ok := stmt.(*ast.CaseClause); ok {
						clauses = append(clauses, clause)
					}
				}
			default:
				return true
			}
			for _, clause := range clauses {
				for _, expr := range clause.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					if name := strings.Trim(lit.Value, `"`); name != "" {
						out = append(out, name)
					}
				}
			}
			return true
		})
		break
	}
	sort.Strings(out)
	return out, nil
}

// publishedTools — имена, которые сервер отдаёт в tools/list.
//
// Читает toolDefs, то есть ровно то, что уходит агенту при discovery. Отдельная
// функция, а не переиспользование dispatch-таблицы: сверять надо две разные
// вещи, и если взять одну вместо другой, проверка станет тавтологией — всегда
// зелёной и всегда бесполезной.
func publishedTools(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name == nil || fn.Name.Name != mcpToolDefs || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Name" {
				return true
			}
			lit, ok := kv.Value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if name := strings.Trim(lit.Value, `"`); name != "" {
				out = append(out, name)
			}
			return true
		})
		break
	}
	sort.Strings(out)
	return out, nil
}

// checkPublishedVsDispatched сверяет объявленный список с принимаемым.
//
// Оба расхождения плохи, но не одинаково:
//
//   - принимается, но не объявлено: агент не найдёт инструмент через discovery
//     и получит отказ без объяснения. Возможность есть, а её не видно;
//   - объявлено, но не принимается: tools/list обещает инструмент, которого
//     нет. Это хуже: агент строит план на несуществующем шаге.
func checkPublishedVsDispatched(published, dispatched []string) []string {
	disp := map[string]bool{}
	for _, t := range dispatched {
		disp[t] = true
	}
	pub := map[string]bool{}
	for _, t := range published {
		pub[t] = true
	}
	var problems []string
	for _, name := range published {
		if !disp[name] {
			problems = append(problems, fmt.Sprintf("    %-22s публикует инструмент %q, которого нет в %s: tools/list обещает несуществующий",
				mcpSchemasFile, name, mcpCallTool))
		}
	}
	for _, name := range dispatched {
		if !pub[name] {
			problems = append(problems, fmt.Sprintf("    %-22s не публикует инструмент %q, который %s принимает: агент не найдёт его через discovery",
				mcpSchemasFile, name, mcpCallTool))
		}
	}
	return problems
}

// checkReadmeTools сверяет строку таблицы README с dispatch-таблицей.
func checkReadmeTools(readme string, tools []string) []string {
	known := map[string]bool{}
	for _, t := range tools {
		known[t] = true
	}
	for _, line := range strings.Split(readme, "\n") {
		if !strings.HasPrefix(line, mcpReadmeRow) {
			continue
		}
		var problems []string
		for _, m := range backtickedRe.FindAllStringSubmatch(line, -1) {
			if !known[m[1]] {
				problems = append(problems, fmt.Sprintf("    %-22s перечисляет инструмент %q, которого нет в %s",
					"README.md", m[1], mcpCallTool))
			}
		}
		for _, name := range tools {
			if !strings.Contains(line, "`"+name+"`") {
				problems = append(problems, fmt.Sprintf("    %-22s не перечисляет инструмент %q, а %s его принимает",
					"README.md", name, mcpCallTool))
			}
		}
		return problems
	}
	return []string{fmt.Sprintf("    %-22s не содержит строки %q — инструменты не перечислены, сверять нечего",
		"README.md", mcpReadmeRow)}
}
