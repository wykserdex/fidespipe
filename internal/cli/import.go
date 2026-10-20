package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wykserdex/fidespipe/internal/core"
	"github.com/wykserdex/fidespipe/internal/importer"
)

// RunPluginImport — fidespipe plugin import <проект> [<каталог>] [--plan]
//
// Разбирает ВНЕШНИЙ Python-проект и создаёт из него каталог плагина.
//
// Что автоматика делает: читает `pyproject.toml` (или `setup.cfg`), находит
// имя, версию, зависимости, точки входа и раскладку, строит план и пишет
// `plugin.yaml`, `main.py`, `plugin.test.yaml`, `README.md`.
//
// Что остаётся человеку: выбрать функцию донора и дописать вызов. Адаптер
// намеренно НЕ вызывает функцию: сигнатура не проверена, а угадывать её —
// значит выдумать работающий вызов. Сгенерированный `main.py` грузит символ и
// останавливается с доменной ошибкой `donor_not_wired`.
//
// --plan печатает разбор и план, НИЧЕГО не записывая.
func RunPluginImport(args []string) {
	if helpRequested(args) {
		printPluginImportUsage()
		return
	}
	var positional []string
	opts := importer.GenerateOptions{}
	planOnly := false
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--id="):
			opts.PluginID = strings.TrimPrefix(a, "--id=")
		case strings.HasPrefix(a, "--description="):
			opts.Description = strings.TrimPrefix(a, "--description=")
		case a == "--plan":
			planOnly = true
		case strings.HasPrefix(a, "-"):
			fmt.Printf("неизвестный флаг %q\n", a)
			os.Exit(2)
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		fmt.Println("нужно: fidespipe plugin import <каталог-проекта> [<каталог-плагина>] [--plan]")
		os.Exit(2)
	}
	projectDir := positional[0]

	project, err := importer.Inspect(projectDir)
	if err != nil {
		fmt.Println("ошибка разбора проекта:", err)
		os.Exit(1)
	}
	plan := importer.Plan(project)

	fmt.Printf("▶ разобран проект: %s\n", project.Name)
	if project.Version != "" {
		fmt.Printf("  версия: %s\n", project.Version)
	}
	if plan.ModuleImportPath != "" {
		fmt.Printf("  раскладка: %s (его надо добавить в sys.path)\n", plan.ModuleImportPath)
	}
	if len(plan.EntryPoints) > 0 {
		names := make([]string, 0, len(plan.EntryPoints))
		for _, e := range plan.EntryPoints {
			names = append(names, e.Name)
		}
		fmt.Printf("  точки входа: %s\n", strings.Join(names, ", "))
	}
	pinned := len(plan.Requirements) - len(plan.Unresolved)
	fmt.Printf("  зависимости: %d (с точным пином %d, без него %d)\n", len(plan.Requirements), pinned, len(plan.Unresolved))
	fmt.Printf("  плагин получится: %s\n", plan.PluginID)

	if planOnly {
		printImportWarnings("что останется на человеке", "что автоматика не осилит", plan.Warnings, plan.Blockers)
		fmt.Println("  ничего не записано (--plan)")
		return
	}

	dest := ""
	if len(positional) > 1 {
		dest = positional[1]
	} else {
		dest = filepath.Join("plugins", plan.PluginID)
	}
	if _, err := os.Stat(dest); err == nil {
		fmt.Printf("каталог %s уже существует — удалите его или укажите другой\n", dest)
		os.Exit(1)
	}

	res, err := importer.Generate(plan, dest, opts)
	if err != nil {
		fmt.Println("ошибка генерации:", err)
		os.Exit(1)
	}
	fmt.Printf("▶ создан плагин %s → %s\n", res.PluginID, res.Dir)
	fmt.Println("  " + strings.Join(res.Files, " + "))

	if errs := core.ValidatePluginDir(res.Dir); len(errs) > 0 {
		fmt.Println("внутренняя ошибка генератора — импортированный плагин невалиден:")
		for _, e := range errs {
			fmt.Println("  ✗", e)
		}
		os.Exit(1)
	}
	fmt.Println("  ✓ манифест валиден")

	passed, failed, terr := core.RunPluginTests(res.Dir, "", false)
	switch {
	case terr != nil:
		fmt.Printf("  · тесты не запустились (%v) — проверьте python3\n", terr)
	case failed > 0:
		fmt.Println("внутренняя ошибка генератора — стартовые тесты красные")
		os.Exit(1)
	default:
		fmt.Printf("  ✓ тесты зелёные (%d/%d)\n", passed, passed+failed)
	}

	printImportWarnings("что останется на человеке", "что автоматика не осилит", res.Warnings, res.Blockers)
	fmt.Println("  дальше: открой редактор (fidespipe gui) и допиши вызов функции донора")
}

// printImportWarnings — печатает предупреждения и блокеры.
//
// Блокеры и предупреждения печатаются ОТДЕЛЬНО и всегда: импорт может
// получиться, но что-то в нём всё равно требует человека, и молчать об
// этому — значит отдать человеку плагин, который не заработает, и не сказать
// об этом.
func printImportWarnings(warnHeader, blockHeader string, warnings, blockers []string) {
	if len(warnings) > 0 {
		fmt.Printf("  ! %s:\n", warnHeader)
		for _, w := range warnings {
			fmt.Println("    ·", w)
		}
	}
	if len(blockers) > 0 {
		fmt.Printf("  ✗ %s:\n", blockHeader)
		for _, b := range blockers {
			fmt.Println("    ·", b)
		}
	}
}

// printPluginImportUsage — справка по команде.
func printPluginImportUsage() {
	fmt.Println(`fidespipe plugin import <каталог-проекта> [<каталог-плагина>] [--id=ИМЯ] [--description=ТЕКСТ] [--plan]

Разбирает внешний Python-проект и создаёт из него каталог плагина.

Читается pyproject.toml (или setup.cfg): имя, версия, зависимости, точки
входа из [project.scripts], раскладка (src/ или плоская).

Пишется: plugin.yaml, main.py, plugin.test.yaml, README.md.

Что автоматика НЕ делает и почему:
  · не превращает диапазон версий в точный пин: из «>=2.6,<3.0» одну версию
    не вывести, не спросив окружение; такое требование попадает в блокеры;
  · не выбирает функцию донора автоматически: сигнатура не проверена, а выдумать
    вызов — значит отдать неработающий плагин. Сгенерированный main.py грузит
    символ и встаёт с доменной ошибкой donor_not_wired;
  · не выполняет setup.py: это чужой исполняемый код.

--plan          напечатать разбор и план, ничего не записывая
--id=ИМЯ        свой id плагина (snake_case)
--description=  своё описание (описание донора не подходит: оно про чужой проект)

Каталог плагина по умолчанию: plugins/<id>.`)
}
