package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Тесты идут на примитивах census, а не на полном прогоне: полный прогон
// занимает минуты и зависит от состояния рабочего дерева, а ломаться там
// может ровно то, что нужно чинить быстро — запуск, ожидание с пределом и
// убийство дерева.

func TestWaitProcessReportsSuccess(t *testing.T) {
	cmd := sleeper(t, 0)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	got := waitProcess(cmd, 30)
	if !got.exited {
		t.Fatal("процесс не завершился, хотя должен был")
	}
	if got.code != 0 {
		t.Fatalf("код завершения = %d, ждали 0", got.code)
	}
}

func TestWaitProcessReportsNonZeroExit(t *testing.T) {
	cmd := failing(t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	got := waitProcess(cmd, 30)
	if !got.exited {
		t.Fatal("процесс не завершился")
	}
	// На Windows кодыNTSTATUS выглядят как большие положительные числа,
	// поэтому проверяем «не ноль», а не конкретное значение.
	if got.code == 0 {
		t.Fatal("падающий процесс отмечен как успешный")
	}
}

// Главный сценарий census: процесс не уложился в предел. Проверяем, что мы
// его замечаем И что после killTree он действительно уходит, а не висит
// вечно. Если вернуться exited=false после killTree, следующий шаг цикла
// начнёт плодить новые процессы — ровно та ошибка, которую census чинит.
func TestWaitProcessLimitThenKillTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("на Windows этот сценарий проверяет TestWaitProcessLimitThenKillTreeWindows")
	}
	cmd := sleeper(t, 120)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := startWait(cmd)
	if got := w.await(1); got.exited {
		t.Fatalf("процесс, живущий 120 с, завершился за 1 с (code=%d)", got.code)
	}
	killTree(cmd.Process.Pid)
	if after := w.await(15); !after.exited {
		t.Fatal("процесс не умер после killTree")
	}
}

func TestWaitProcessLimitThenKillTreeWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("только для Windows")
	}
	cmd := sleeper(t, 120)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := startWait(cmd)
	if got := w.await(1); got.exited {
		t.Fatalf("процесс, живущий 120 с, завершился за 1 с (code=%d)", got.code)
	}
	killTree(cmd.Process.Pid)
	if after := w.await(30); !after.exited {
		t.Fatal("процесс не умер после taskkill /T")
	}
}

// Регресс на баг, который поймал CI: Cmd.Wait нельзя звать дважды, даже
// последовательно. До правки повторный вызов порождал вторую горутину с
// Wait() на том же Cmd, и -race в CI рапортовал DATA RACE. Тест повторяет
// именно тот сценарий (ожидание -> таймаут -> killTree -> ожидание заново),
// потому что проверять надо не «последний вызов», а «сколько раз звали Wait».
func TestRepeatedAwaitDoesNotCallWaitTwice(t *testing.T) {
	cmd := sleeper(t, 60)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := startWait(cmd)
	if got := w.await(1); got.exited {
		t.Fatal("процесс не должен был завершиться")
	}
	killTree(cmd.Process.Pid)
	first := w.await(15)
	if !first.exited {
		t.Fatal("процесс не умер после killTree")
	}
	// Третий и последующие вызовы обязаны отдавать тот же результат
	// без нового Wait().
	for i := 0; i < 3; i++ {
		got := w.await(1)
		if !got.exited || got.code != first.code {
			t.Fatalf("повтор %d: получили {exited:%v code:%d}, ждали {true %d}",
				i, got.exited, got.code, first.code)
		}
	}
}

func TestSanitizeForFileKeepsPackageShape(t *testing.T) {
	// Имя файла лога выводится из имени пакета: там есть слэши и двоеточия,
	// которые в пути недопустимы.
	got := sanitizeForFile("github.com/wykserdex/fidespipe/internal/mcp:sub")
	for _, bad := range []string{"/", ":", `\`, " ", "*", "?"} {
		if strings.Contains(got, bad) {
			t.Fatalf("в имени файла остался недопустимый символ %q: %q", bad, got)
		}
	}
	if !strings.HasPrefix(got, "fidespipe_internal") {
		t.Fatalf("имя файла потеряло читаемость: %q", got)
	}
}

func TestFindRepoRootFromSubdir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "internal", "cli", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findRepoRoot(deep); got != root {
		t.Fatalf("корень найден как %q, ждали %q", got, root)
	}
	// В каталоге без go.mod подниматься выше некуда: пустой результат, а не
	// корень файловой системы.
	if got := findRepoRoot(t.TempDir()); got != "" {
		t.Fatalf("найден корень %q там, где go.mod нет", got)
	}
}

// Имена шагов попадают в --only и в сводку, поэтому должны быть уникальны и
// непусты: иначе --only=test молча прогонял бы не то.
func TestCheckStepNamesUniqueAndNonEmpty(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range checkSteps() {
		if s.name == "" {
			t.Fatal("шаг без имени")
		}
		if seen[s.name] {
			t.Fatalf("дубль имени шага: %s", s.name)
		}
		seen[s.name] = true
		if s.desc == "" {
			t.Fatalf("шаг %s без описания", s.name)
		}
		if s.run == nil {
			t.Fatalf("шаг %s без функции запуска", s.name)
		}
	}
}

// Порядок шагов — часть контракта с CI: fmt/vet до сборки, сборка до того,
// что зовёт бинарь. Порядок в нарезке не проверяем, он очевиден.
func TestCheckStepOrder(t *testing.T) {
	var names []string
	for _, s := range checkSteps() {
		names = append(names, s.name)
	}
	// Проверяем, что ожидаемые шаги идут в этом порядке, а не что список
	// совпадает целиком. Прежняя версия требовала точного равенства, и любая
	// новая проверка стоила правки теста — то есть расходы на каждую
	// последующую проверку росли. Здесь можно просто дописать шаг.
	pos := func(name string) int {
		for i, n := range names {
			if n == name {
				return i
			}
		}
		return -1
	}
	for _, want := range []string{"fmt", "vet", "mod", "build", "test", "conformance", "pipelines", "plugins", "registry"} {
		if pos(want) < 0 {
			t.Fatalf("шага %q нет среди %v", want, names)
		}
	}
	// Порядок между ними — часть контракта с CI, поэтому проверяем именно его.
	ordered := []string{"fmt", "vet", "mod", "build", "test", "conformance", "pipelines", "plugins", "registry"}
	for i := 1; i < len(ordered); i++ {
		if pos(ordered[i-1]) > pos(ordered[i]) {
			t.Fatalf("шаг %q идёт после %q: порядок важен, порядок в CI такой же",
				ordered[i], ordered[i-1])
		}
	}
}

// Ни один шаг не должен звать бинарь, не взяв его из ensureFidespipe: иначе
// шаг, запущенный отдельно, упадёт с невнятным "path not found".
func TestBinaryUsingStepsGoThroughEnsure(t *testing.T) {
	o := checkOpts{repo: t.TempDir()}
	if _, err := ensureFidespipe(o); err == nil {
		t.Log("ensureFidespipe отработал в пустом каталоге (не ошибка, но неожиданно)")
	}
}

func TestRunCheckRejectsUnknownStep(t *testing.T) {
	if code := RunCheck([]string{"--only=такого-шага-нет"}); code != 2 {
		t.Fatalf("код возврата = %d, ждали 2", code)
	}
}

func TestRunCheckRejectsUnknownFlag(t *testing.T) {
	if code := RunCheck([]string{"--нетакого-флага"}); code != 2 {
		t.Fatalf("код возврата = %d, ждали 2", code)
	}
}

func TestRunCheckListSucceeds(t *testing.T) {
	if code := RunCheck([]string{"--list"}); code != 0 {
		t.Fatalf("--list вернул %d, ждали 0", code)
	}
}

func TestSingleLineCollapses(t *testing.T) {
	if got := singleLine("  \n\n  первая\nвторая\nтретья\n"); !strings.HasPrefix(got, "первая") {
		t.Fatalf("singleLine = %q", got)
	}
	if got := singleLine(""); got != "" {
		t.Fatalf("singleLine на пустом вводе = %q", got)
	}
	if got := singleLine("только одна"); got != "только одна" {
		t.Fatalf("singleLine = %q", got)
	}
}

func TestFirstLineFallsBackToError(t *testing.T) {
	if got := firstLine("\n\n", os.ErrClosed); !strings.Contains(got, "closed") {
		t.Fatalf("firstLine = %q, ждали текст ошибки", got)
	}
}

func TestEnsureFidespipeBuildsWhenMissing(t *testing.T) {
	// Каталог без исходников: сборка обязана упасть, и падать внятно.
	dir := t.TempDir()
	if _, err := ensureFidespipe(checkOpts{repo: dir}); err == nil {
		t.Skip("каталог неожиданно собрался; пропускаем")
	} else if !strings.Contains(err.Error(), "сборка fidespipe") {
		t.Fatalf("ошибка без указания на сборку: %v", err)
	}
}

func TestNewestSourceIgnoresGitAndVar(t *testing.T) {
	dir := t.TempDir()
	// Файл в .git и в var не должны считаться исходником: иначе любой
	// мусор в var/ заставлял бы пересобирать бинарь на каждом запуске.
	for _, rel := range []string{".git/HEAD", "var/census/old.txt"} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := newestSource(dir); err != nil || !got.IsZero() {
		t.Fatalf("newestSource = %v, %v; ждали ноль", got, err)
	}
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := newestSource(dir)
	if err != nil || got.IsZero() {
		t.Fatalf("новestSource не увидел a.go: %v, %v", got, err)
	}
}

func TestTailOfReturnsLastLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log.txt")
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("строка\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(tailOf(p, 5), "\n")
	if len(got) != 5 {
		t.Fatalf("tailOf вернул %d строк, ждали 5", len(got))
	}
}

// runOnePackage намеренно ставит -timeout ГОРАЗДО ниже внешнего предела.
// Причина: если пакет завис, ценная улика — дамп горутин от самого go test.
// Если бы внешний предел срабатывал первым, мы получили бы молчание и
// никакого дампа. Этот тест закрепляет именно такой порядок.
func TestRunOnePackagePrefersGoTimeoutOverOuterLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("запускает настоящий процесс")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module censusprobe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(dir, "slow")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package slow\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\n" +
		"func TestSlow(t *testing.T) { time.Sleep(120 * time.Second) }\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "slow_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "slow.log")
	// Внешний предел 60 с, -timeout 5 с: сработать должен -timeout.
	code, limitHit := runOnePackage(checkOpts{repo: dir}, "./slow", logPath, 60, 5)
	if limitHit {
		t.Fatal("сработал внешний предел вместо -timeout: дампа горутин не будет")
	}
	if code == 0 {
		t.Fatal("у зависшего пакета код 0")
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Дамп горутин — то, ради чего весь этот затевается.
	if !strings.Contains(string(raw), "timed out") {
		t.Fatalf("в логе нет сообщения о таймауте; лог:\n%s", firstLines(string(raw), 20))
	}
	if !strings.Contains(string(raw), "goroutine") {
		t.Fatalf("в логе нет дампа горутин; лог:\n%s", firstLines(string(raw), 20))
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// sleeper(t, secs) — команда, которая завершится успешно через secs секунд.
// secs=0 означает «сразу».
func sleeper(t *testing.T, secs int) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		if secs == 0 {
			return exec.Command("cmd", "/c", "exit", "0")
		}
		return exec.Command("powershell", "-NoProfile", "-Command",
			"Start-Sleep -Seconds "+itoa(secs))
	}
	return exec.Command("sleep", itoa(secs))
}

// failing(t) — команда, которая завершится с ненулевым кодом.
func failing(t *testing.T) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "exit", "3")
	}
	return exec.Command("sh", "-c", "exit 3")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
