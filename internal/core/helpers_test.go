package core

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/plugin"
)

// requirePython пропускает интеграционные тесты без интерпретатора.
func requirePython(t *testing.T) {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if _, err := exec.LookPath(name); err == nil {
			return
		}
	}
	t.Skip("python не найден — пропускаю интеграционный тест")
}

// newStdin подменяет os.Stdin скриптом ответов (для тестов human_gate).
func newStdin(t *testing.T, data string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(data); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
}

// readEvents читает journal.jsonl прогона построчно; любая битая строка = падение теста
// (журнал — контракт, он обязан быть валидным JSONL всегда).
func readEvents(t *testing.T, runDir string) []map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(runDir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("битая строка журнала %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

func countEvents(events []map[string]interface{}, typ string) int {
	n := 0
	for _, e := range events {
		if e["type"] == typ {
			n++
		}
	}
	return n
}

// quietOpts — базовые опции рана для тестов.
//
// Доверие выдаётся ЯВНО (по хэшу содержимого) каталогам фикстур. После
// инверсии доверия (H1) плагин из testdata — внешний код, и ран без allow-list
// падал бы на отказе по политике вместо той проверки, ради которой написан.
//
// Тот же приём, что в `fidespipe plugin test <dir>`: каталог назван человеком
// (здесь — тестом), а не объявлен самим плагином.
func quietOpts(t *testing.T) RunOptions {
	t.Helper()
	return RunOptions{Quiet: true, RunsDir: t.TempDir(), Trusted: fixtureAllowList(t)}
}

// fixtureAllowList — allow-list по каталогам фикстур ядра.
func fixtureAllowList(t *testing.T) *plugin.AllowList {
	t.Helper()
	list, err := plugin.AllowListFromDirs(fixtureRoots()...)
	if err != nil {
		t.Fatalf("allow-list фикстур: %v", err)
	}
	return list
}

// fixtureRoots — каталоги с плагинами-фикстурами, которые гоняют тесты ядра.
//
// Кроме testdata и конформности сюда входит plugins/ — тесты ядра запускают и
// штатные плагины репозитория (llm_plugins_test, bind_test). Их рабочее
// содержимое отличается от того, что лежит по пину реестра, поэтому доверенным
// оно становится здесь, по хэшу того, что реально лежит на диске.
func fixtureRoots() []string {
	roots := []string{filepath.Join("testdata", "plugins")}
	if cwd, err := os.Getwd(); err == nil {
		roots = append(roots,
			filepath.Join(cwd, "conformance", "fixtures", "v0.2"),
			filepath.Join(cwd, "plugins"))
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		dir := filepath.Dir(file)
		roots = append(roots,
			filepath.Join(dir, "testdata", "plugins"),
			filepath.Join(dir, "..", "..", "conformance", "fixtures", "v0.2"),
			filepath.Join(dir, "..", "..", "plugins"))
	}
	return roots
}

// execPlugin / execPluginEnv — запуск плагина-фикстуры с политикой доверия
// фикстур. Обёртки нужны потому, что обычный plugin.Exec* идёт с пустым ctx, а
// после инверсии доверия (H1) это «никто не доверен»: тест получал бы отказ
// по политике вместо проверки поведения.
func execPlugin(m *Manifest, input []byte, timeout time.Duration) *ExecResult {
	return execPluginEnv(m, input, timeout, nil)
}

func execPluginEnv(m *Manifest, input []byte, timeout time.Duration, extraEnv []string) *ExecResult {
	pr := plugin.ExecWithEnvCtx(fixtureTrustCtx(), m, input, timeout, extraEnv)
	return fromPluginRes(pr)
}

// fixtureTrustCtx — ctx с allow-list по всем каталогам фикстур. Считается
// один раз: каталоги фикстур не меняются в пределах прогона тестов.
var fixtureTrustOnce struct {
	sync.Once
	ctx context.Context
}

func fixtureTrustCtx() context.Context {
	fixtureTrustOnce.Do(func() {
		list, err := plugin.AllowListFromDirs(fixtureRoots()...)
		if err != nil {
			// Каталогов может не быть (тест запущен не из дерева ядра) — тогда
			// пустой allow-list и fail-closed, а не panic.
			list = plugin.NewAllowList()
		}
		fixtureTrustOnce.ctx = plugin.WithTrustPolicy(context.Background(), plugin.TrustPolicy{Trusted: list})
	})
	return fixtureTrustOnce.ctx
}
