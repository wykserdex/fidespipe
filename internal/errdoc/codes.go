// Package errdoc сверяет коды ошибок, которые код выдаёт, с кодами, которые
// объявлены публичным контрактом protocol/v0.2/ERRORS.md.
//
// Проверка существует потому, что расхождение этого класса уже случалось
// трижды и каждый раз молча: агент получает код в issues[].code, обязан по
// нему чинить YAML, а в контракте кода нет. Или наоборот — контракт обещает
// код, который никогда не эмитится.
//
// Четыре группы расхождений:
//
//	(а) объявлен в Go и описан в документе — норма.
//	(б) объявлен и используется, но не описан — дыра: агент не найдёт код.
//	(в) описан в документе, но нигде не объявлен и не выдаётся литералом —
//	    дыра в другую сторону: обещано то, чего не будет.
//	(г) объявлен, но не используется. Подразделяется на «мёртвый» (нигде не
//	    описан) и «зарезервированный» (описан глобом, но не эмитится) —
//	    зарезервированный не считается поломкой, но печатается.
//	(л) выдаётся строковым литералом, а не константой: работает, но опечатку
//	    в нём не ловит ничто.
//
// Никаких зависимостей: go/ast, go/parser, os, path/filepath, strings, sort.
package errdoc

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

const (
	// issueFile — где объявляются коды валидации.
	issueFile = "internal/pipeline/issue.go"
	// errorsDoc — публичный контракт.
	errorsDoc = "protocol/v0.2/ERRORS.md"
)

var (
	// codeRe — сам код, целиком совпадающий с содержимым литерала.
	codeRe = regexp.MustCompile(`^[EW]_[A-Z][A-Z0-9_]*$`)
	// codeInsideRe — код ВНУТРИ произвольной строки: fmt.Errorf с
	// префиксом кода либо сырой JSON с полем code. Границы слова нужны,
	// чтобы E_X не ловил E_XY.
	codeInsideRe = regexp.MustCompile(`\b[EW]_[A-Z][A-Z0-9_]*\b`)
)

type usage struct {
	Where string
	Prod  bool // не из _test.go
}

type inventory struct {
	// declared: код → "issue.go:88"
	declared map[string]string
	// documented: код → "ERRORS.md:70" либо "(глоб E_MANIFEST_*)"
	documented map[string]string
	// usages: код → вхождения
	usages map[string][]usage
	// literalOnly: коды, встречающиеся строковым литералом вне issue.go,
	// значение — где встретились (для отчёта об опечатке).
	literalOnly map[string]string
}

// CheckRepo сверяет коды с контрактом. Отчёт возвращается строкой, а решение —
// через error: так шаг fidespipe check покажет и короткую сводку, и подробности.
func CheckRepo(repo string) (string, error) {
	inv, err := scan(repo)
	if err != nil {
		return "", err
	}
	// Страховка от вакуумно-зелёной проверки: пустой результат разбора — это
	// поломка САМОЙ проверки (переехали файлы, сломался парсер), а не успех.
	// Без этой проверки «зелёный CI» означал бы «проверка ничего не нашла».
	if len(inv.declared) == 0 {
		return "", fmt.Errorf("%s: не найдено ни одной константы кода — проверка сломана, а не прошла", issueFile)
	}
	if len(inv.documented) == 0 {
		return "", fmt.Errorf("%s: в таблицах не найдено ни одного кода — проверка сломана, а не прошла", errorsDoc)
	}

	var missingDoc, missingCode, dead, reserved, literal []string
	var unknownLiteral, needsConst []string
	for code, at := range inv.declared {
		_, documented := inv.documented[code]
		used := len(inv.usages[code]) > 0
		switch {
		case used && !documented:
			missingDoc = append(missingDoc, fmt.Sprintf("    %-28s объявлен %s, эмитится %s, в %s нет",
				code, at, firstProd(inv.usages[code]), errorsDoc))
		case !used && !documented:
			dead = append(dead, fmt.Sprintf("    %-28s объявлен %s, не используется и не описан", code, at))
		case !used && documented:
			reserved = append(reserved, fmt.Sprintf("    %-28s описан (%s), но не эмитится",
				code, inv.documented[code]))
		}
	}
	for code, at := range inv.documented {
		if _, ok := inv.declared[code]; ok {
			continue
		}
		if _, emitted := inv.literalOnly[code]; emitted {
			continue
		}
		missingCode = append(missingCode, fmt.Sprintf("    %-28s описан %s, но нигде не объявлен и не выдаётся", code, at))
	}
	for code, at := range inv.literalOnly {
		if _, ok := inv.declared[code]; ok {
			continue
		}
		// ЛИТЕРАЛ БЕЗ ОПРЕДЕЛЕНИЯ. Код, которого нет ни среди констант, ни
		// среди строк контракта, — это почти всегда опечатка: агент получит
		// код, которого нет ни в одном документе, и найти его не сможет. Именно
		// этот случай раньше ловился ничем, потому что литерал не сверялся ни
		// с чем.
		if _, known := inv.documented[code]; !known {
			unknownLiteral = append(unknownLiteral,
				fmt.Sprintf("    %-28s встречается как строка (%s), но нет ни константы, ни строки в %s",
					code, at, errorsDoc))
			continue
		}
		// Известный код, но всё ещё строкой. Не поломка: работает и описан.
		// Но опечатку в соседнем литерале такой код уже не защитит, поэтому
		// показываем как «требует константы».
		needsConst = append(needsConst, code)
	}
	sort.Strings(missingDoc)
	sort.Strings(missingCode)
	sort.Strings(dead)
	sort.Strings(reserved)
	sort.Strings(literal)
	sort.Strings(unknownLiteral)
	sort.Strings(needsConst)

	var b strings.Builder
	fmt.Fprintf(&b, "коды ошибок: в коде %d, в контракте %d; не описаны %d, без кода %d, мёртвых %d, зарезервировано %d, литералом %d, неизвестных литералов %d",
		len(inv.declared), len(inv.documented), len(missingDoc), len(missingCode), len(dead), len(reserved), len(literal), len(unknownLiteral))

	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		b.WriteString("\n\n" + title)
		for _, s := range lines {
			b.WriteString("\n" + s)
		}
	}
	section(fmt.Sprintf("(б) выдаётся, но не описан в %s:", errorsDoc), missingDoc)
	section(fmt.Sprintf("(в) описан в %s, но не объявлен и не выдаётся:", errorsDoc), missingCode)
	section("(г) мёртвые константы — объявлены, не описаны, не используются:", dead)
	section("(р) зарезервировано — описано, но не эмитится (это не поломка):", reserved)
	section("(х) строковый литерал без определения — почти наверняка опечатка:", unknownLiteral)
	section("(л) известный код, но пока строкой, а не константой — работает, опечатку рядом не защитит:", needsConst)

	if len(missingDoc) == 0 && len(missingCode) == 0 && len(dead) == 0 && len(unknownLiteral) == 0 {
		return b.String(), nil
	}
	return b.String(), fmt.Errorf("коды расходятся: не описаны %d, без кода %d, мёртвых %d, неизвестных литералов %d",
		len(missingDoc), len(missingCode), len(dead), len(unknownLiteral))
}

func firstProd(us []usage) string {
	for _, u := range us {
		if u.Prod {
			return u.Where
		}
	}
	if len(us) > 0 {
		return us[0].Where + " (только тест)"
	}
	return "?"
}

func scan(repo string) (*inventory, error) {
	inv := &inventory{
		declared:    map[string]string{},
		documented:  map[string]string{},
		usages:      map[string][]usage{},
		literalOnly: map[string]string{},
	}
	if err := inv.parseIssueFile(repo); err != nil {
		return nil, err
	}
	if err := inv.parseDoc(repo); err != nil {
		return nil, err
	}
	if err := inv.scanGo(repo); err != nil {
		return nil, err
	}
	return inv, nil
}

// parseIssueFile вычитывает объявления констант через AST, а не регуляркой:
// регулярка откатится молча, если отступ или формат поменяются.
func (inv *inventory) parseIssueFile(repo string) error {
	path := filepath.Join(repo, filepath.FromSlash(issueFile))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return fmt.Errorf("разобрать %s: %w", issueFile, err)
	}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if code := strings.Trim(lit.Value, `"`); codeRe.MatchString(code) {
					inv.declared[code] = fmt.Sprintf("%s:%d", issueFile, fset.Position(lit.Pos()).Line)
				}
			}
		}
	}
	return nil
}

// parseDoc вычитывает коды из таблиц ERRORS.md. Ячейка с `*` на конце — глоб
// и покрывает все коды с таким префиксом: `E_MANIFEST_*` означает всю семью,
// иначе семь зарезервированных кодов выглядели бы как семь дыр.
func (inv *inventory) parseDoc(repo string) error {
	raw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(errorsDoc)))
	if err != nil {
		return fmt.Errorf("прочитать %s: %w", errorsDoc, err)
	}
	// Глобы запоминаем отдельно: объявления уже разобраны, но порядок
	// обхода документа к ним отношения не имеет.
	var globs []struct{ prefix, at string }
	for i, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 2 {
			continue
		}
		cell := strings.TrimSpace(cells[1])
		cell = strings.Trim(cell, "`")
		switch {
		case strings.HasSuffix(cell, "*"):
			globs = append(globs, struct{ prefix, at string }{
				strings.TrimSuffix(cell, "*"), fmt.Sprintf("%s:%d (глоб %s)", errorsDoc, i+1, cell)})
		case codeRe.MatchString(cell):
			inv.documented[cell] = fmt.Sprintf("%s:%d", errorsDoc, i+1)
		}
	}
	for _, g := range globs {
		for code := range inv.declared {
			if strings.HasPrefix(code, g.prefix) {
				inv.documented[code] = g.at
			}
		}
	}
	return nil
}

// scanGo обходит internal/ и cmd/ и считает вхождения кодов: идентификаторы
// (использование константы) и строковые литералы (код, объявленный строкой).
//
// Комментарии не разбираются: parser вызывается без ParseComments, поэтому
// упоминание кода в пояснении не считается использованием. Иначе ссылка на
// код в комментарии делала бы мёртвую константу живой.
func (inv *inventory) scanGo(repo string) error {
	for _, root := range []string{"internal", "cmd"} {
		base := filepath.Join(repo, root)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if n := d.Name(); n == ".git" || n == "var" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			// Тесты пропускаем: фикстуры этой же проверки содержат вымышленные
			// коды, и их нельзя считать эмиссией. Побочно это правильно и для
			// проверки мёртвых: константа, которую видит только тест, мертва.
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			return inv.scanFile(repo, path)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (inv *inventory) scanFile(repo, path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return fmt.Errorf("разобрать %s: %w", path, err)
	}
	rel, relErr := filepath.Rel(repo, path)
	if relErr != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)
	prod := !strings.HasSuffix(path, "_test.go")
	// issue.go — место ОБЪЯВЛЕНИЯ, там код встречается как литерал, но
	// использованием не является.
	isDeclFile := rel == issueFile

	ast.Inspect(f, func(n ast.Node) bool {
		// ast.Inspect вызывает функцию с nil после обхода поддерева.
		// Позиция вычисляется до проверки типа, поэтому nil обязан быть
		// отсечён здесь же, иначе паника на n.Pos().
		if n == nil {
			return false
		}
		at := fmt.Sprintf("%s:%d", rel, fset.Position(n.Pos()).Line)
		switch x := n.(type) {
		case *ast.Ident:
			if !codeRe.MatchString(x.Name) {
				return true
			}
			// Имя константы — это тоже Ident, и оно совпадает с шаблоном
			// кода. Если его засчитать, каждая константа окажется
			// «использованной» сама собой и проверка мёртвых кодов не
			// сработает НИКОГДА. Пропускаем только имя, совпадающее с
			// объявлением, и только в файле объявлений.
			if isDeclFile {
				if _, isDecl := inv.declared[x.Name]; isDecl {
					return true
				}
			}
			inv.usages[x.Name] = append(inv.usages[x.Name], usage{Where: at, Prod: prod})
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			s := strings.Trim(x.Value, `"`)
			if !codeRe.MatchString(s) {
				// Не сам код, а строка, ВНУТРИ которой код встречается:
				// fmt.Errorf("E_X: ...") или сырой JSON вида
				// {"error":"...","code":"E_SESSION_REQUIRED"}. Без этого
				// такие коды выглядели бы невыдающимися.
				for _, code := range codeInsideRe.FindAllString(s, -1) {
					inv.usages[code] = append(inv.usages[code], usage{Where: at, Prod: prod})
					if !isDeclFile {
						if _, seen := inv.literalOnly[code]; !seen {
							inv.literalOnly[code] = at
						}
					}
				}
				return true
			}
			// Объявление константы — это не использование. Если его
			// засчитать, проверка мёртвых кодов не сработает НИКОГДА: каждая
			// константа «использована» сама собой. Это был бы вакуумно-зелёный
			// шаг, то есть худший вид поломки проверки.
			if isDeclFile {
				return true
			}
			inv.usages[s] = append(inv.usages[s], usage{Where: at, Prod: prod})
			if _, seen := inv.literalOnly[s]; !seen {
				inv.literalOnly[s] = at
			}
		}
		return true
	})
	return nil
}
