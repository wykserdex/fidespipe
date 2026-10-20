package main

import (
	"flag"
	"fmt"
	"github.com/wykserdex/fidespipe/internal/compat"
	"os"
	"sort"
	"strings"

	"github.com/wykserdex/fidespipe/internal/buildinfo"
	"github.com/wykserdex/fidespipe/internal/core"
	"github.com/wykserdex/fidespipe/internal/journal"
)

func usage() {
	fmt.Fprintf(os.Stderr, "fidespipe tool v%s — инструмент плагинов (контракт: PROTOCOL.md)\n", buildinfo.Resolve())
	fmt.Fprintln(os.Stderr, `
  tool run <pipeline.yaml> [--yes] [--runs <dir>] [--resume <run_id>]   запуск цепочки
                                                     --yes: auto-accept human_gate (CI/демо)
                                                     --resume: продолжить с последнего item
  tool validate <pipeline.yaml>                     статическая проверка цепочки
  tool plugin validate <dir>                        проверка манифеста плагина
  tool plugin test <dir> [--spec file.yaml]         контракт-тесты из plugin.test.yaml
  tool plugin create <path> [--author N] [--description ".."] [--example string|array]  скелет плагина (сразу зелёный)
  tool plugin list                                        список плагинов official/community
  tool plugin search <query>                              поиск по id/описанию/автору
  tool runs list                                    список прогонов var/runs/
  tool runs show <run_id>                           журнал + context

  exit-коды run: 0 — ран доехал до конца; 1 — рановая неудача
  (платформенная ошибка; в одиночном режиме — также stop/reject)`)
	os.Exit(2)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func report(errs, warns []string) {
	for _, w := range warns {
		fmt.Println("  · предупреждение:", w)
	}
	for _, e := range errs {
		fmt.Println("  ✗", e)
	}
}

func loadPipeline(path string) (*core.PipelineFile, error) {
	return core.LoadPipelineFile(path)
}

func main() {
	compat.Init()
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "validate":
		validateCmd(os.Args[2:])
	case "runs":
		if len(os.Args) < 3 {
			runsListCmd()
		} else {
			switch os.Args[2] {
			case "list":
				runsListCmd()
			case "show":
				if len(os.Args) < 4 {
					usage()
				}
				runsShowCmd(os.Args[3])
			default:
				runsShowCmd(os.Args[2])
			}
		}
	case "plugin":
		if len(os.Args) < 3 {
			usage()
		}
		switch os.Args[2] {
		case "validate":
			pluginValidateCmd(os.Args[3:])
		case "test":
			pluginTestCmd(os.Args[3:])
		case "create":
			pluginCreateCmd(os.Args[3:])
		case "list":
			toolPluginList()
		case "search":
			toolPluginSearch(os.Args[3:])
		default:
			usage()
		}
	case "plugin-validate":
		pluginValidateCmd(os.Args[2:])
	case "plugin-test":
		pluginTestCmd(os.Args[2:])
	case "plugin-create":
		pluginCreateCmd(os.Args[2:])
	default:
		usage()
	}
}

func runsListCmd() {
	// делегируем в journal reader
	dir := "var/runs"
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Println("нет прогонов:", err)
		return
	}
	fmt.Printf("Прогоны в %s:\n", dir)
	for _, e := range entries {
		if e.IsDir() {
			fmt.Println(" ", e.Name())
		}
	}
}

func runsShowCmd(id string) {
	dir, err := journal.SafeRunDir("var/runs", id)
	if err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(1)
	}
	raw, err := os.ReadFile(dir + "/journal.jsonl")
	if err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(1)
	}
	fmt.Println(string(raw))
	snap, err := os.ReadFile(dir + "/context.json")
	if err == nil {
		fmt.Println("\n--- context.json ---")
		fmt.Println(string(snap))
	}
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	yes := fs.Bool("yes", false, "auto-accept human_gate")
	runsDir := fs.String("runs", "var/runs", "каталог журналов прогонов")
	resume := fs.String("resume", "", "продолжить прогон с run_id")

	var positional, flags []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-yes", "--yes", "-y":
			flags = append(flags, "-yes")
		case "-runs", "--runs":
			if i+1 < len(args) {
				flags = append(flags, "-runs", args[i+1])
				i++
			}
		case "-resume", "--resume":
			if i+1 < len(args) {
				flags = append(flags, "-resume", args[i+1])
				i++
			}
		default:
			if len(args[i]) > 9 && args[i][:9] == "--resume=" {
				flags = append(flags, "-resume", args[i][9:])
			} else {
				positional = append(positional, args[i])
			}
		}
	}
	_ = fs.Parse(append(flags, positional...))
	if fs.NArg() < 1 {
		usage()
	}

	pf, err := loadPipeline(fs.Arg(0))
	fatal(err)

	eng := core.NewEngine()
	errs, warns := core.Validate(pf, eng)
	report(errs, warns)
	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "цепочка не прошла статическую валидацию")
		os.Exit(1)
	}

	stats, err := core.Run(pf, eng, core.RunOptions{Yes: *yes, RunsDir: *runsDir, Resume: *resume})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ОШИБКА РАНА:", err)
		os.Exit(1)
	}
	// одиночный ран, остановленный доменной ошибкой или reject'ом — неуспех для CI
	if pf.Pipeline.Foreach == "" && stats.Aborted > 0 {
		os.Exit(1)
	}
}

func validateCmd(args []string) {
	if len(args) < 1 {
		usage()
	}
	pf, err := loadPipeline(args[0])
	fatal(err)
	errs, warns := core.Validate(pf, core.NewEngine())
	report(errs, warns)
	if len(errs) > 0 {
		os.Exit(1)
	}
	fmt.Println("OK: цепочка совместима")
}

func pluginValidateCmd(args []string) {
	if len(args) < 1 {
		usage()
	}
	errs := core.ValidatePluginDir(args[0])
	for _, e := range errs {
		fmt.Println("  ✗", e)
	}
	if len(errs) > 0 {
		os.Exit(1)
	}
	fmt.Println("OK: манифест плагина корректен")
}

func pluginTestCmd(args []string) {
	if len(args) < 1 {
		usage()
	}
	dir := args[0]
	spec := ""
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--spec" {
			spec = args[i+1]
			i++
		}
	}
	fmt.Printf("▶ plugin test %s\n", dir)
	passed, failed, err := core.RunPluginTests(dir, spec, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
	fmt.Printf("■ plugin test: %d passed, %d failed\n", passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// pluginCreateCmd создаёт скелет и СРАЗУ доказывает его работоспособность:
// валидация манифеста + прогон стартовых контракт-тестов.
func pluginCreateCmd(args []string) {
	dir, opts, listRequested, err := core.ParseCreateArgs(args) // флаги в любом порядке
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		usage()
	}
	if listRequested {
		fmt.Print(core.ListTemplates())
		return
	}
	id, err := core.CreatePluginWith(dir, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
	fmt.Printf("▶ создан плагин %s → %s\n", id, dir)
	fmt.Println("  plugin.yaml + main.py + plugin.test.yaml + README.md")

	if errs := core.ValidatePluginDir(dir); len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "внутренняя ошибка генератора — скелет невалиден:")
		report(errs, nil)
		os.Exit(1)
	}
	fmt.Println("  ✓ манифест валиден")

	passed, failed, err := core.RunPluginTests(dir, "", false)
	if err != nil {
		fmt.Printf("  · тесты не запустились (%v) — проверьте python3 и запустите: tool plugin test %s\n", err, dir)
	} else if failed > 0 {
		fmt.Fprintln(os.Stderr, "внутренняя ошибка генератора — стартовые тесты красные")
		os.Exit(1)
	} else {
		fmt.Printf("  ✓ %d стартовых теста зелёные\n", passed)
	}

	fmt.Printf("\nДальше: правьте %s/main.py, затем tool plugin test %s\n", dir, dir)
}

// v0.17: list/search — те же команды, что в cmd/fidespipe (CI проверяет tool)
func toolPluginList() {
	plugins := core.ScanPlugins()
	if len(plugins) == 0 {
		fmt.Println("плагины не найдены в plugins/official и plugins/community")
		return
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].ID < plugins[j].ID })
	fmt.Printf("Найдено %d плагинов:\n", len(plugins))
	for _, p := range plugins {
		fmt.Printf("  %-24s %-10s %s (%s)\n", p.ID, p.Version, p.Dir, p.Description)
	}
}

func toolPluginSearch(args []string) {
	if len(args) < 1 {
		fmt.Println("нужен запрос: tool plugin search <query>")
		os.Exit(2)
	}
	query := strings.ToLower(strings.Join(args, " "))
	plugins := core.ScanPlugins()
	matched := []core.Manifest{}
	for _, p := range plugins {
		hay := strings.ToLower(p.ID + " " + p.Description + " " + p.Author)
		if strings.Contains(hay, query) {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		fmt.Printf("по запросу %q ничего не найдено (всего %d плагинов)\n", query, len(plugins))
		return
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })
	fmt.Printf("По запросу %q найдено %d:\n", query, len(matched))
	for _, p := range matched {
		fmt.Printf("  %-24s %-10s %s\n    %s\n", p.ID, p.Version, p.Dir, p.Description)
	}
}
