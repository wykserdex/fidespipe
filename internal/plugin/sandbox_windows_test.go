//go:build windows

package plugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// TestWindowsSandboxIsolates — враждебная проверка изолятора на Windows.
//
// Проверяется не «плагин отработал», а то, чего он НЕ может: писать в свой
// каталог, видеть каталог без выдачи и выйти в сеть. Проверка «просто
// запустилось» доказывала бы только то, что запустилось.
//
// Каталог без выдачи берётся СОСЕДНИМ с каталогом плагина, а не где-то в
// стороне: иначе «недостижим» окажется свойством расположения, а не
// отсутствия выдачи.
//
// Сеть проверяется настоящим слушателем на хосте: подключение к закрытому
// порту отличить от запрета нельзя — оба дают исключение.
func TestWindowsSandboxIsolates(t *testing.T) {
	if !sandboxUsable() {
		t.Skip("песочница недоступна на этом хосте")
	}
	requirePython(t)

	dir := t.TempDir()
	denied := t.TempDir()
	if err := os.WriteFile(filepath.Join(denied, "secret.txt"), []byte("секрет"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Слушатель на loopback: подключиться к нему сможет только тот, кому
	// разрешена сеть.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("слушатель: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	script := `import json, os, socket, sys
out = {}
def try_write(path):
    try:
        with open(path, "w") as f:
            f.write("x")
        return "OK"
    except Exception as e:
        return type(e).__name__
def try_list(path):
    try:
        return sorted(os.listdir(path))
    except Exception as e:
        return type(e).__name__
here = os.path.dirname(os.path.abspath(__file__))
listing = try_list(here)
out["plugin_list_count"] = len(listing) if isinstance(listing, list) else -1
out["plugin_write"] = try_write(os.path.join(here, "intruder.txt"))
scratch = os.environ.get("TMPDIR") or os.environ.get("TEMP") or "."
out["scratch_write"] = try_write(os.path.join(scratch, "mine.txt"))
out["denied_read"] = try_list(os.environ["DENIED_DIR"])
try:
    s = socket.create_connection(("127.0.0.1", int(os.environ["DENIED_PORT"])), timeout=3)
    s.close()
    out["socket"] = "OK"
except Exception as e:
    out["socket"] = type(e).__name__
sys.stdout.write(json.dumps({"status": "ok", "output": out}))
`
	m := writePlugin(t, dir, script)
	extra := []string{
		"DENIED_DIR=" + denied,
		fmt.Sprintf("DENIED_PORT=%d", port),
	}

	res := ExecWithEnvCtx(AllowUntrustedPlugins(context.Background()), m, []byte("{}"), 60*time.Second, extra)
	if !res.OK() {
		t.Fatalf("плагин в контейнере должен отработать: %+v (stderr: %s)", res, res.Stderr)
	}
	out, _ := res.Output["plugin_write"].(string)
	if out == "OK" {
		t.Fatal("изолятор не заблокировал запись в каталог плагина")
	}
	// «Только на чтение» должно отличаться от «недостижим»: если каталог
	// плагина вообще не читается, запрет на запись ничего не доказывает.
	if n, _ := res.Output["plugin_list_count"].(float64); n <= 0 {
		t.Fatalf("каталог плагина должен читаться, а listing дал %v", res.Output["plugin_list_count"])
	}
	if s, _ := res.Output["scratch_write"].(string); s != "OK" {
		t.Fatalf("scratch должен быть записываемым, получено %q", s)
	}
	// Каталог без выдачи обязан быть НЕДОСТИЖИМ. try_list при отказе
	// возвращает имя исключения, а при успехе — список; значит успех читается
	// как список, и именно он означает дыру.
	if listing, ok := res.Output["denied_read"].([]any); ok {
		t.Fatalf("каталог без выдачи должен быть недостижим, а плагин его прочитал: %v", listing)
	}
	if deniedRead, _ := res.Output["denied_read"].(string); deniedRead == "" {
		t.Fatalf("отказ должен быть объяснён, а не пуст: %v", res.Output)
	}
	if s, _ := res.Output["socket"].(string); s == "OK" {
		t.Fatal("изолятор не заблокировал сеть: плагин подключился к слушателю на хосте")
	}
	if _, err := os.Stat(filepath.Join(dir, "intruder.txt")); err == nil {
		t.Fatal("плагин смог создать файл в своём каталоге на хосте")
	}
}

// TestWindowsSandboxRefusesNetworkDeclaration — плагин с объявленной сетью на
// Windows не запускается.
//
// Не «запускается без сети», а именно не запускается: сеть была обещана
// плагину в манифесте, и выполнить обещание нечем. Молчаливый запуск без сети
// выглядел бы как «плагин сломался», а отказ говорит прямо.
func TestWindowsSandboxRefusesNetworkDeclaration(t *testing.T) {
	if !sandboxUsable() {
		t.Skip("песочница недоступна на этом хосте")
	}
	dir := t.TempDir()
	m := writePlugin(t, dir, "import sys\nsys.stdout.write('{\"status\":\"ok\",\"output\":{}}')\n")
	// any_host — единственный исполнимый вид объявленной сети: сеть по списку
	// хостов отсекает runner, ещё до песочницы (см. sandbox.go).
	m.Permissions = pipeline.Permissions{Network: []pipeline.NetworkPermission{{AnyHost: true}}}
	_, _, _, err := sandboxArgs(m, []string{"python"}, t.TempDir())
	if err == nil {
		t.Fatal("плагин с объявленной сетью обязан быть отвергнут на Windows")
	}
}

// TestWindowsSandboxReadPaths — что именно контейнеру выдаётся.
//
// Список выдач это и есть граница изоляции, поэтому он проверяется текстом:
// каталог плагина и каталог интерпретатора обязаны быть, а пустой список
// означал бы, что плагин не запустится вовсе.
func TestWindowsSandboxReadPaths(t *testing.T) {
	dir := t.TempDir()
	m := &pipeline.Manifest{ID: "x", Runtime: pipeline.Runtime{Type: "python"}, Sandbox: pipeline.SandboxUntrusted, Dir: dir}
	paths := sandboxReadPaths(m, []string{`C:\Python314\python.exe`})
	found := false
	for _, p := range paths {
		if p == dir {
			found = true
		}
	}
	if !found {
		t.Fatalf("каталог плагина должен быть в выдаче: %v", paths)
	}
	interp := false
	for _, p := range paths {
		if p == `C:\Python314` {
			interp = true
		}
	}
	if !interp {
		t.Fatalf("каталог интерпретатора должен быть в выдаче: %v", paths)
	}
}
