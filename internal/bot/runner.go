package bot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner выполняет пайплайны движком. ExtraPATH — каталог console-скриптов
// Python (см. ScriptsDir), который подставляется в PATH дочернего процесса.
type Runner struct {
	Bin       string
	Root      string
	RunsRoot  string
	ExtraPATH string
	sem       chan struct{}
}

func NewRunner(bin, root, runsRoot, extraPath string, maxConcurrent int) *Runner {
	return &Runner{Bin: bin, Root: root, RunsRoot: runsRoot, ExtraPATH: extraPath, sem: make(chan struct{}, maxConcurrent)}
}

// scriptsProbe — путь, куда pip кладёт исполняемые скрипты этого интерпретатора.
// На Windows это Scripts рядом с python.exe, на POSIX — тот же bin/, где сам
// интерпретатор.
const scriptsProbe = "import os, sys; print(os.path.join(os.path.dirname(sys.executable)," +
	" 'Scripts' if os.name == 'nt' else '.'))"

// ScriptsDir — каталог console-скриптов донорского Python.
//
// Зачем: ядро передаёт плагинам PATH и узкий список системных переменных, а
// произвольные переменные не пропускает — иначе окружение пользователя
// протекало бы во внешний код. Поэтому HOLEHE_BIN плагину не виден, и
// единственный легальный способ познакомить плагин с holehe.exe — PATH.
// На этой машине pip кладёт скрипты в …\pythoncore-3.14-64\Scripts, которого
// в PATH нет, и шаг holehe честно падал в skip: платный отчёт терял источник,
// о котором покупатель не знал.
//
// Пустой результат — не ошибка: донор может стоять в venv на PATH, тогда
// добавлять нечего.
func ScriptsDir(pythonBin string) string {
	if pythonBin == "" {
		return ""
	}
	cmd := exec.Command(pythonBin, "-c", scriptsProbe)
	// Без UTF-8 питон печатает путь в кодировке консоли (cp1251 на русской
	// Windows), и у пользователя с кириллицей в имени профиля каталог не
	// находится: байты не совпадают, os.Stat даёт ошибку.
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return ""
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return filepath.Clean(dir)
}

// pathWith — ставит доп. каталог в начало PATH среди переменных окружения.
func pathWith(extra string, env []string) []string {
	if extra == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	found := false
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(name, "PATH") {
			out = append(out, name+"="+extra+string(os.PathListSeparator)+value)
			found = true
			continue
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+extra)
	}
	return out
}

// Run выполняет пайплайн и возвращает каталог прогона. RunsRoot/orderID —
// отдельный каталог на заказ, поэтому run dir находится без разбора stdout:
// внутри ровно один подкаталог, и гонки двух параллельных заказов нет.
func (r *Runner) Run(ctx context.Context, orderID, pipelineFile, inputKey, target string) (string, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	dir := filepath.Join(r.RunsRoot, orderID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	args := []string{
		"pipeline", "run", filepath.Join("product", "pipelines", pipelineFile),
		"--input=" + inputKey + "=" + target,
		"--runs-dir=" + dir,
	}
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = r.Root
	// Окружение наследуем: урезаем не всё, а только PATH — плагины ищут
	// доноров по PATH (см. ScriptsDir), остальное ядро фильтрует само.
	cmd.Env = pathWith(r.ExtraPATH, os.Environ())
	out, err := cmd.CombinedOutput()
	runDir, findErr := singleSubdir(dir)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("таймаут прогона (%s)", pipelineFile)
		}
		return "", fmt.Errorf("движок: %w\n%s", err, truncate(string(out), 600))
	}
	if findErr != nil {
		return "", findErr
	}
	return runDir, nil
}

func singleSubdir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("движок не создал каталог прогона в %s", dir)
}

func RunWithTimeout(r *Runner, parent context.Context, timeout time.Duration, orderID, pipelineFile, inputKey, target string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return r.Run(ctx, orderID, pipelineFile, inputKey, target)
}
