package cli

// Режим census: `fidespipe check --census`.
//
// Обычный `go test ./...` на Windows-раннере ведёт себя плохо: пакет, который
// завис, тянет за собой весь набор, и шаг молчит до джобового таймаута. Здесь
// пакеты идут поштучно, у каждого СВОЙ предел и своя строка прогресса, поэтому
// первый зависший видно сразу, а не через двадцать минут тишины.
//
// Зависший пакет не просто убивается: снимается состояние (процессы с
// родителями, состояние потоков) и, если система это умеет, скриншот. Потому
// что «молчал и умер» и «молчал, потому что встал системный вызов» —
// разные причины с разными лекарствами.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type pkgOutcome struct {
	pkg      string
	secs     float64
	code     int
	limitHit bool
	tail     string
}

func runCensus(o checkOpts) (string, error) {
	listOut, err := runCapture(o.repo, "go", "list", "./...")
	if err != nil {
		return listOut, fmt.Errorf("go list ./...: %w", err)
	}
	var pkgs []string
	for _, l := range strings.Split(listOut, "\n") {
		s := strings.TrimSpace(l)
		if s != "" {
			pkgs = append(pkgs, s)
		}
	}
	if len(pkgs) == 0 {
		return "", fmt.Errorf("go list не назвал ни одного пакета")
	}
	if o.pkgFilter != "" {
		var kept []string
		for _, p := range pkgs {
			if strings.Contains(p, o.pkgFilter) {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			return "", fmt.Errorf("под фильтр %q не подошёл ни один пакет", o.pkgFilter)
		}
		pkgs = kept
	}
	sort.Strings(pkgs)
	logDir := filepath.Join(o.repo, "var", "census")
	_ = os.MkdirAll(logDir, 0o755)

	fmt.Printf("   пакетов: %d\n", len(pkgs))
	outcomes := make([]pkgOutcome, 0, len(pkgs))
	start := time.Now()
	for _, pkg := range pkgs {
		safe := sanitizeForFile(pkg)
		pkgLog := filepath.Join(logDir, safe+".txt")
		t0 := time.Now()
		code, limitHit := runOnePackage(o, pkg, pkgLog, o.perPackageSec, o.goTimeoutSec)
		took := time.Since(t0)
		tail := tailOf(pkgLog, 20)
		oc := pkgOutcome{pkg: pkg, secs: took.Round(time.Millisecond).Seconds(), code: code, limitHit: limitHit, tail: tail}
		outcomes = append(outcomes, oc)
		switch {
		case limitHit:
			fmt.Fprintf(os.Stderr, "   ПРЕДЕЛ %-38s %6.1fs  превышен %d с, дерево убито\n", pkg, oc.secs, o.perPackageSec)
			captureHangEvidence(pkg, pkgLog, logDir, oc)
		case code != 0:
			fmt.Fprintf(os.Stderr, "   FAIL   %-38s %6.1fs  exit=%d\n", pkg, oc.secs, code)
			for _, l := range strings.Split(tail, "\n") {
				if strings.TrimSpace(l) != "" {
					fmt.Fprintln(os.Stderr, "        |", l)
				}
			}
		default:
			fmt.Printf("   ok     %-38s %6.1fs\n", pkg, oc.secs)
		}
	}

	failed, limited := 0, 0
	var problems []string
	for _, oc := range outcomes {
		if oc.limitHit {
			limited++
			failed++
			problems = append(problems, oc.pkg+" (предел)")
		} else if oc.code != 0 {
			failed++
			problems = append(problems, oc.pkg+" (exit="+strconv.Itoa(oc.code)+")")
		}
	}
	summary := fmt.Sprintf("итого: пакетов=%d, не прошли=%d (из них по пределу=%d), времени=%s",
		len(outcomes), failed, limited, time.Since(start).Round(time.Second))
	if failed > 0 {
		return summary, fmt.Errorf("не прошли: %s", strings.Join(problems, ", "))
	}
	return summary, nil
}

// runOnePackage запускает `go test` для одного пакета с пределом на весь
// процесс. Предел выше лимита самого go test: сначала должен сработать
// -timeout, чтобы мы получили дамп горутин (это самая ценная улика), и лишь
// потом внешний предел.
func runOnePackage(o checkOpts, pkg, logPath string, perPackageSec, goTimeoutSec int) (code int, limitHit bool) {
	limit := perPackageSec
	if goTimeoutSec+30 > limit {
		limit = goTimeoutSec + 30
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return -1, false
	}
	defer logFile.Close()
	cmd := goTestCommand(o.repo, pkg, goTimeoutSec, logFile)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logFile, "не удалось запустить go test: %v\n", err)
		return -1, false
	}
	// Process.Release() здесь НЕЛЬЗЯ звать: после него Wait() всегда
	// возвращает ошибку, и удачный тест был бы записан как exit=-1.
	// Освобождение ресурсов делает сам Wait.
	w := startWait(cmd)
	waited := w.await(limit)
	if !waited.exited {
		killTree(cmd.Process.Pid)
		// Повторное ожидание читает тот же результат Wait(), а не зовёт
		// его заново: второй Wait() на том же Cmd — гонка данных.
		_ = w.await(15)
		return -1, true
	}
	return waited.code, false
}

func tailOf(path string, lines int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	all := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// pkgShortName убирает домен и владельца из пути пакета, оставляя имя модуля:
// github.com/wykserdex/fidespipe/internal/mcp → fidespipe/internal/mcp.
//
// Это не косметика: раньше имя модуля было ровно "fidespipe", и весь путь пакета уже
// читался коротко. После переезда на полный путь модуля (нужный, чтобы работал
// `go install`) имя файла стало "github.com_wykserdex_fidespipe_internal_mcp" — а имя
// шага census (`var/census/<пакет>.txt`) человек читает глазами и ищет в нём
// пакет, а не репозиторий.
//
// Правило не знает ни одного конкретного модуля: если первый сегмент пути похож
// на домен (содержит точку), он и следующий за ним сегмент отбрасываются.
func pkgShortName(pkg string) string {
	parts := strings.Split(pkg, "/")
	if len(parts) < 3 || !strings.Contains(parts[0], ".") {
		return pkg
	}
	return strings.Join(parts[2:], "/")
}

func sanitizeForFile(pkg string) string {
	s := pkgShortName(pkg)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// captureHangEvidence снимает то, что успевает, пока система жива: дерево
// процессов с родителями и, на Windows, скриншот рабочего стола. Ни то ни
// другое не должно падать: если машина зависла целиком, это тоже результат.
func captureHangEvidence(pkg, pkgLog, logDir string, oc pkgOutcome) {
	dir := filepath.Join(logDir, "HANG-"+sanitizeForFile(pkg))
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "log.txt"), []byte(oc.tail), 0o644)

	var report string
	if isWindows() {
		report, _ = runCapture(".", "powershell", "-NoProfile", "-Command", windowsProcessTablePS)
	} else {
		report, _ = runCapture(".", "ps", "-eo", "pid=,ppid=,etimes=,args=")
	}
	if report == "" {
		report = "не удалось снять таблицу процессов"
	}
	_ = os.WriteFile(filepath.Join(dir, "procs.txt"), []byte(report), 0o644)

	if isWindows() {
		if shot, err := windowsScreenshot(); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "screen.png"), shot, 0o644)
		}
	}
	fmt.Fprintf(os.Stderr, "   улицы: %s\n", dir)
}

const windowsProcessTablePS = `Get-CimInstance Win32_Process | Sort-Object ProcessId | ForEach-Object { "pid={0} ppid={1} {2} :: {3}" -f $_.ProcessId,$_.ParentProcessId,$_.Name,$_.CommandLine }`
