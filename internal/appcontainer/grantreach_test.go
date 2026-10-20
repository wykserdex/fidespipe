//go:build windows

package appcontainer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGrantReachesFiles — после выдачи прав контейнер читает файл из дерева.
//
// РЕГРЕССИЯ. Windows пересчитывает наследование по дереву НЕ мгновенно:
// сразу после SetNamedSecurityInfo файл внутри каталога может ещё не иметь
// унаследованной записи, и процесс в контейнере получает «Permission denied».
// Измерено: первый запуск плагина после выдачи прав падал именно так, а
// второй (когда пересчёт уже завершился) проходил.
//
// Поэтому выдача ждёт, пока права дойдут до файлов, — и этот тест проверяет
// именно наблюдаемое свойство: контейнер ЧИТАЕТ файл, а не «в DCL есть
// запись».
func TestGrantReachesFiles(t *testing.T) {
	py, err := realPython()
	if err != nil {
		t.Skipf("интерпретатор не найден: %v", err)
	}
	// Каталог с файлом внутри — как каталог плагина.
	dir := t.TempDir()
	target := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(target, []byte("данные"), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	script := filepath.Join(scratch, "read.py")
	body := `import sys
try:
    with open(sys.argv[1], "rb") as f:
        data = f.read()
    result = "READ_OK %d" % len(data)
except Exception as e:
    result = "READ_FAIL " + type(e).__name__ + " " + str(e)
open(sys.argv[2], "w").write(result)
`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	name := ProfileName("grantreach")
	out := filepath.Join(scratch, "out.txt")
	_, runErr := Run(context.Background(), Plan{
		Name: name,
		Argv: []string{py, script, target, out},
		// Каталог интерпретатора тоже выдаётся: без него контейнерный Python
		// не стартует вовсе, и проверять было бы нечего.
		Read:    Readable{dir, filepath.Dir(py)},
		Scratch: scratch,
		WorkDir: scratch,
		Env:     []string{"PATH=" + os.Getenv("PATH")},
	})
	if runErr != nil {
		t.Fatalf("запуск в контейнере: %v", runErr)
	}
	res, rerr := readResult(scratch)
	if rerr != nil {
		t.Fatalf("результата нет: %v", rerr)
	}
	if !strings.HasPrefix(res, "READ_OK") {
		t.Fatalf("контейнер не прочитал файл из выданного каталога: %s", res)
	}
}

// readResult — читает результат самопроверки из scratch.
func readResult(scratch string) (string, error) {
	data, err := os.ReadFile(filepath.Join(scratch, "out.txt"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// realPython — настоящий интерпретатор, а не шимб менеджера пакетов.
func realPython() (string, error) {
	for _, name := range []string{"python3", "python"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		out, err := exec.Command(p, "-X", "utf8", "-c", "import sys; print(sys.executable)").Output()
		if err != nil {
			continue
		}
		candidate := strings.TrimSpace(string(out))
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("интерпретатор python не найден")
}
