package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/journal"
)

// Регрессия: каталог прогонов выбирался по-разному у писателя и у читателя.
// `pipeline run` вне репозитория Fidespipe писал в runs/ (var/runs там не
// существует), а `runs list`/`runs show` искали строго var/runs — только что
// записанный ран был не виден вообще: список пуст, `runs show` падал
// «журнал не найден».
func TestRunsListSeesRunWrittenOutsideRepo(t *testing.T) {
	dir := t.TempDir() // ни var/runs, ни git: каталог, в котором живёт не-репозиторий
	t.Chdir(dir)

	if got := journal.DefaultRunsDir(); got != "runs" {
		t.Fatalf("в каталоге без var/runs ожидался runs, получено %q", got)
	}

	id := "20260101-000000-selftest-abc123"
	store := journal.NewFilesystemStore("")
	j, err := store.Create(id)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	if err := j.Event("run_start", map[string]interface{}{"pipeline": "selftest"}); err != nil {
		t.Fatalf("event: %v", err)
	}

	// Ран обязан лежать там, куда смотрит чтение, — иначе он потерян.
	if _, err := os.Stat(filepath.Join(dir, "runs", id)); err != nil {
		t.Fatalf("ран не записан в выбранный каталог: %v", err)
	}

	listed := listRunsForTest(t, "")
	if !strings.Contains(listed, id) {
		t.Fatalf("runs list не увидел рана %s:\n%s", id, listed)
	}
	if strings.Contains(listed, "var/runs") {
		t.Fatalf("runs list смотрит в var/runs вместо runs:\n%s", listed)
	}

	// Тот же каталог обязан использоваться и чтением журнала конкретного рана:
	// SafeRunDir с пустым baseDir — это ровно тот путь, который зовёт `runs show`.
	dirOfRun, err := journal.SafeRunDir("", id)
	if err != nil {
		t.Fatalf("SafeRunDir(\"\") вернул ошибку: %v", err)
	}
	if want := filepath.Join(dir, "runs", id); dirOfRun != want {
		t.Fatalf("SafeRunDir = %q, want %q", dirOfRun, want)
	}
	events, err := journal.NewReader(dirOfRun).Events()
	if err != nil {
		t.Fatalf("чтение журнала: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("журнал рана прочитан пустым")
	}
}

// Тот же сценарий в репозитории: раз var/runs есть, выбирается он, и фолбэк не
// должен молча уводить прогоны в другое место.
func TestRunsDirPrefersVarRunsWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "var", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if got, want := journal.DefaultRunsDir(), filepath.Join("var", "runs"); got != want {
		t.Fatalf("DefaultRunsDir = %q, want %q", got, want)
	}

	id := "20260101-000000-selftest-var0001"
	store := journal.NewFilesystemStore("")
	j, err := store.Create(id)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	if _, err := os.Stat(filepath.Join("var", "runs", id)); err != nil {
		t.Fatalf("ран не записан в var/runs: %v", err)
	}
	if _, err := os.Stat("runs"); err == nil {
		t.Fatal("фолбэк создал runs/ при наличии var/runs")
	}

	if listed := listRunsForTest(t, ""); !strings.Contains(listed, id) {
		t.Fatalf("runs list не увидел рана в var/runs:\n%s", listed)
	}
}

// Явный путь оператора побеждает правило по умолчанию.
func TestRunsListRespectsExplicitDir(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom-runs")
	id := "20260101-000000-selftest-custom1"
	j, err := journal.NewFilesystemStore(custom).Create(id)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { j.Close() })
	t.Chdir(dir)

	listed := listRunsForTest(t, custom)
	if !strings.Contains(listed, id) {
		t.Fatalf("runs list --runs-dir не увидел рана %s:\n%s", id, listed)
	}
}

// listRunsForTest повторяет разбор флагов RunRunsList и возвращает напечатанное:
// сама команда пишет в os.Stdout и ничего не возвращает.
func listRunsForTest(t *testing.T, explicit string) string {
	t.Helper()
	args := []string{}
	if explicit != "" {
		args = append(args, "--runs-dir="+explicit)
	}
	return captureStdout(t, func() { RunRunsList(args) })
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		chunk := make([]byte, 4096)
		for {
			n, rerr := r.Read(chunk)
			buf = append(buf, chunk[:n]...)
			if rerr != nil {
				break
			}
		}
		done <- string(buf)
	}()
	fn()
	os.Stdout = old
	w.Close()
	out := <-done
	r.Close()
	return out
}
