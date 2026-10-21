package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/wykserdex/fidespipe/internal/core"
	"github.com/wykserdex/fidespipe/internal/execution"
	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

func runIDFromDir(runDir string) string {
	if runDir == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(runDir))
}

// trustConfigPath — где искать конфиг доверия оператора.
//
// Конфиг доверия решает, чей код получит права пользователя, поэтому искать
// его в рабочем каталоге нельзя: этот каталог задаёт не оператор, а тот, кто
// положил сюда пайплайн. Клон репозитория с приложенным fidespipe-trust.yaml
// проходил все проверки N3 (файл принадлежит пользователю, права 0644) и молча
// расширял allow-list — при том, что доверие оператора выдавалось командой,
// запущенной внутри чужого дерева.
//
// Порядок поиска, от сильного к слабому:
//
//  1. --trust-config=<путь> — оператор назвал файл сам;
//  2. $FIDESPIPE_TRUST_CONFIG — то же для CI и скриптов, где флаг не передать;
//  3. каталог бинаря: %ProgramFiles%\fidespipe\fidespipe-trust.yaml и т.п. Лежит там,
//     куда писать может только установщик, а не код из клона.
//
// Рабочий каталог в списке нет намеренно. Если файл там лежит, но не был
// выбран, печатается явное предупреждение с готовой командой: молча игнорировать
// конфиг нельзя (оператор решит, что доверяет, а на деле — нет), и молча
// читать его тоже нельзя (это и есть дыра).
//
// Отсутствие конфига — не ошибка: возвращается пустой путь, LoadTrustConfig
// трактует его как «оператор ничего не добавил» и берёт встроенный allow-list.
func trustConfigPath(override string) string {
	if override != "" {
		return override
	}
	if env := strings.TrimSpace(os.Getenv("FIDESPIPE_TRUST_CONFIG")); env != "" {
		return env
	}
	candidate, ok := executableTrustConfigPath()
	if ok {
		if path, found := plugin.TrustConfigFileExists(filepath.Dir(candidate)); found {
			return path
		}
	}
	warnIgnoredWorkingDirTrustConfig()
	if !ok {
		// Каталог бинаря определить нечем. Возвращать относительный путь нельзя:
		// это снова конфиг из чужого рабочего каталога. Пустой путь читается
		// как «конфига нет» и даёт встроенный allow-list — то есть fail-closed.
		return ""
	}
	return candidate
}

// executableTrustConfigPath — конфиг рядом с бинарём.
func executableTrustConfigPath() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	return filepath.Join(filepath.Dir(exe), plugin.TrustConfigFile), true
}

// warnIgnoredWorkingDirTrustConfig — файл доверия в рабочем каталоге не
// используется, и об этом нужно сказать явно.
//
// Молчать нельзя с двух сторон: проигнорированный конфиг выглядит как
// «оператор доверяет», а это неправда; и без предупреждения миграция на новый
// путь выглядит как поломка без причины.
func warnIgnoredWorkingDirTrustConfig() {
	abs, err := filepath.Abs(plugin.TrustConfigFile)
	if err != nil {
		return
	}
	if exePath, ok := executableTrustConfigPath(); ok {
		if exeAbs, err := filepath.Abs(exePath); err == nil && exeAbs == abs {
			return // тот же файл, что и каталог бинаря — предупреждать не о чем
		}
	}
	if _, err := os.Stat(plugin.TrustConfigFile); err != nil {
		return // файла нет — предупреждать не о чем
	}
	fmt.Fprintf(os.Stderr,
		"ВНИМАНИЕ: %s в рабочем каталоге НЕ используется как конфиг доверия.\n"+
			"  Рабочий каталог может принадлежать не вам (клон репозитория, распакованный архив),\n"+
			"  а конфиг доверия решает, чей код получит ваши права.\n"+
			"  Подключить его явно: --trust-config=%s\n"+
			"  Постоянно: задайте FIDESPIPE_TRUST_CONFIG=%s\n",
		plugin.TrustConfigFile, abs, abs)
}

func RunPipelineRun(args []string) {
	file := ""
	yes := false
	runsDir := ""
	resume := ""
	store := "fs"
	dbPath := ""
	noAuto := false
	denyUntrusted := false
	allowUntrusted := false
	trustCfg := ""
	// inputOverrides — значения входов, вписанные человеком: «--input key=value».
	// Это то, чем «принёс файл» обходится без правки YAML: путь к своему
	// файлу человек знает, а формат пайплайна — нет.
	inputOverrides := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--yes":
			yes = true
		case a == "--no-auto-approve":
			noAuto = true
		case a == "--deny-untrusted-plugins":
			denyUntrusted = true
		case a == "--allow-untrusted-plugins":
			allowUntrusted = true
		case strings.HasPrefix(a, "--input="):
			k, v, ok := strings.Cut(strings.TrimPrefix(a, "--input="), "=")
			if !ok || strings.TrimSpace(k) == "" {
				fmt.Println("флагу --input нужно значение вида key=value")
				os.Exit(2)
			}
			inputOverrides[k] = v
		case a == "--input":
			if i+1 >= len(args) {
				fmt.Println("флагу --input нужно значение вида key=value")
				os.Exit(2)
			}
			i++
			k, v, ok := strings.Cut(args[i], "=")
			if !ok || strings.TrimSpace(k) == "" {
				fmt.Println("флагу --input нужно значение вида key=value")
				os.Exit(2)
			}
			inputOverrides[k] = v
		case strings.HasPrefix(a, "--trust-config="):
			trustCfg = strings.TrimPrefix(a, "--trust-config=")
		case a == "--trust-config":
			if i+1 >= len(args) {
				fmt.Println("флагу --trust-config нужно значение")
				os.Exit(2)
			}
			i++
			trustCfg = args[i]
		case strings.HasPrefix(a, "--runs-dir="):
			runsDir = strings.TrimPrefix(a, "--runs-dir=")
		case a == "--runs-dir":
			if i+1 >= len(args) {
				fmt.Println("флагу --runs-dir нужно значение")
				os.Exit(2)
			}
			i++
			runsDir = args[i]
		case strings.HasPrefix(a, "--resume="):
			resume = strings.TrimPrefix(a, "--resume=")
		case a == "--resume":
			if i+1 >= len(args) {
				fmt.Println("флагу --resume нужно значение")
				os.Exit(2)
			}
			i++
			resume = args[i]
		case strings.HasPrefix(a, "--store="):
			store = strings.TrimPrefix(a, "--store=")
		case a == "--store":
			if i+1 >= len(args) {
				fmt.Println("флагу --store нужно значение")
				os.Exit(2)
			}
			i++
			store = args[i]
		case strings.HasPrefix(a, "--db-path="):
			dbPath = strings.TrimPrefix(a, "--db-path=")
		case a == "--db-path":
			if i+1 >= len(args) {
				fmt.Println("флагу --db-path нужно значение")
				os.Exit(2)
			}
			i++
			dbPath = args[i]
		case strings.HasPrefix(a, "-"):
			fmt.Printf("неизвестный флаг pipeline run %q\n", a)
			os.Exit(2)
		case file == "":
			file = a
		default:
			fmt.Printf("лишний аргумент pipeline run %q\n", a)
			os.Exit(2)
		}
	}
	if file == "" {
		fmt.Println("нужен файл пайплайна: orchestrator pipeline run <file.yaml>")
		os.Exit(2)
	}
	if runsDir == "" {
		runsDir = journal.DefaultRunsDir()
	}

	eng := core.NewEngine()
	pf, err := core.LoadPipelineFile(file)
	if err != nil {
		fmt.Println("ошибка загрузки пайплайна:", err)
		os.Exit(2)
	}
	if len(inputOverrides) > 0 {
		if err := applyInputOverrides(pf, inputOverrides); err != nil {
			fmt.Println("входы:", err)
			os.Exit(2)
		}
	}
	trusted, err := plugin.EffectiveAllowList(trustConfigPath(trustCfg))
	if err != nil {
		fmt.Println("ошибка конфига доверия:", err)
		os.Exit(2)
	}
	errs, warns := core.Validate(pf, eng)
	for _, w := range warns {
		fmt.Println("  · предупреждение:", w)
	}
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Println("  ✗", e)
		}
		os.Exit(1)
	}
	// v0.9: первый Ctrl+C — graceful cancel (журнал run_cancelled + snapshot,
	// --resume поднимет), второй — жёсткий выход.
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Println("\n  ! Ctrl+C — отменяю ран (ещё раз — выйти сразу)")
		cancel()
		<-sig
		os.Exit(130)
	}()
	stats, err := core.Run(pf, eng, core.RunOptions{Yes: yes, RunsDir: runsDir, Resume: resume, Store: store, DBPath: dbPath, Ctx: runCtx, NoAutoApprove: noAuto, DenyUntrusted: denyUntrusted, AllowUntrusted: allowUntrusted, Trusted: trusted})
	if err != nil {
		if errors.Is(err, execution.ErrCancelled) {
			fmt.Println("ран отменён; продолжить: fidespipe runs resume", runIDFromDir(stats.RunDir))
			os.Exit(130)
		}
		fmt.Println("ран упал:", err)
		os.Exit(1)
	}
	if code := runExitCode(pf.Pipeline.Foreach, stats); code != 0 {
		os.Exit(code)
	}
}

// runExitCode — PROTOCOL §6: `0` — ран дошёл до конца (per-item итоги в
// журнале), `1` — рановая неудача: платформенная ошибка (сюда не доходит,
// её обрабатывает вызывающий по err) либо, в одиночном режиме без foreach,
// stop/reject. В батче (foreach) частичные aborts — это per-item результат
// (`item_aborted` в журнале), а не рановая неудача: список из 10 000 строк,
// где три битые, дошёл до конца, и CI не должен видеть по нему отказ.
func runExitCode(foreach string, stats execution.RunStats) int {
	if foreach == "" && stats.OK == 0 {
		return 1
	}
	if stats.Aborted > 0 && foreach == "" {
		return 1
	}
	return 0
}

// applyInputOverrides — подставляет входы, вписанные человеком в CLI.
//
// Переопределяются только объявленные пресетом входы: неизвестный ключ —
// ошибка, а не тихое «проверилось и ничего не нашлось».
//
// Поле, которое пресет перебирает (foreach), получает список: значение
// режется по строкам, чтобы «--input file=a.txt» не превратилось в один
// элемент списка из нескольких путей.
func applyInputOverrides(pf *core.PipelineFile, values map[string]string) error {
	// Перебираемое поле в пресете одно (foreach — строка), и именно оно
	// список: значение режется по строкам.
	listFields := map[string]bool{}
	if ref := pf.Pipeline.Foreach; strings.HasPrefix(ref, "input.") {
		listFields[strings.TrimPrefix(ref, "input.")] = true
	}
	for k, v := range values {
		if _, ok := pf.Pipeline.Input[k]; !ok {
			return fmt.Errorf("вход %q у пресета не объявлен", k)
		}
		if listFields[k] {
			items := []string{}
			for _, line := range strings.Split(v, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					items = append(items, line)
				}
			}
			pf.Pipeline.Input[k] = items
			continue
		}
		pf.Pipeline.Input[k] = v
	}
	return nil
}
