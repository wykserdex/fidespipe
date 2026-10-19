package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// untrustedManifestWith — python-плагин, который на запуск пишет marker.
func sandboxPlugin(t *testing.T, dir, marker string) *pipeline.Manifest {
	t.Helper()
	script := "import sys\n" +
		"open(" + quotePy(marker) + ", 'w').write('ran')\n" +
		"sys.stdout.write('{\"status\":\"ok\",\"output\":{}}')\n"
	return writePlugin(t, dir, script)
}

func writePlugin(t *testing.T, dir, script string) *pipeline.Manifest {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "plugin.py"), []byte(script), 0o600); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	return &pipeline.Manifest{
		ID:      "sandbox-test",
		Version: "0.1.0",
		Runtime: pipeline.Runtime{Type: "python", Entry: "plugin.py"},
		Sandbox: pipeline.SandboxUntrusted,
		Dir:     dir,
	}
}

func TestSandboxBackendReported(t *testing.T) {
	name := sandboxBackendName()
	if name == "нет" {
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			return // ожидаемо: платформа без изолятора
		}
		t.Skip("изолятор не установлен в этом раннере")
	}
	if name != "bwrap" && name != "sandbox-exec" {
		t.Fatalf("неизвестный изолятор %q", name)
	}
}

func TestPlatformSandboxFailsClosedWithoutBackend(t *testing.T) {
	if sandboxUsable() {
		t.Skip("песочница на этом хосте работает — проверяется в TestUntrustedRunsInsideSandbox")
	}
	m := &pipeline.Manifest{ID: "x", Runtime: pipeline.Runtime{Type: "python"}, Sandbox: pipeline.SandboxUntrusted, Dir: t.TempDir()}
	_, _, _, err := sandboxArgs(m, []string{"/bin/true"}, t.TempDir())
	if !errors.Is(err, ErrSandboxUnsupported) {
		t.Fatalf("без рабочей песочницы ожидался ErrSandboxUnsupported, получено: %v", err)
	}
}

func TestSandboxArgsKeepPluginCommandLast(t *testing.T) {
	if !sandboxUsable() {
		t.Skip("песочница на этом хосте не работает — аргументы не собрать")
	}
	m := &pipeline.Manifest{ID: "x", Runtime: pipeline.Runtime{Type: "python"}, Sandbox: pipeline.SandboxUntrusted, Dir: t.TempDir()}
	launcher, args, _, err := sandboxArgs(m, []string{"/usr/bin/python3", "/plug/plugin.py"}, t.TempDir())
	if err != nil {
		t.Fatalf("sandboxArgs: %v", err)
	}
	if launcher == "" || len(args) == 0 {
		t.Fatal("launcher или аргументы пусты")
	}
	tail := args[len(args)-2:]
	if tail[0] != "/usr/bin/python3" || tail[1] != "/plug/plugin.py" {
		t.Fatalf("команда плагина должна быть последней, получено: %v", args)
	}
}

func TestUntrustedRefusedWhenBackendMissing(t *testing.T) {
	if sandboxUsable() {
		t.Skip("песочница на этом хосте работает")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	m := sandboxPlugin(t, dir, marker)
	res := ExecWithEnvCtx(AllowUntrustedPlugins(context.Background()), m, []byte("{}"), 5*time.Second, nil)
	if res.OK() || res.ErrCode != "sandbox_unavailable" {
		t.Fatalf("без песочницы запуск untrusted обязан быть отвергнут: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("процесс был запущен без изоляции")
	}
}

func TestUntrustedRunsInsideSandbox(t *testing.T) {
	if !sandboxUsable() {
		t.Skip("песочница недоступна на этом хосте (проверяется отказ в TestUntrustedRefusedWhenBackendMissing)")
	}
	requirePython(t)
	dir := t.TempDir()
	// Плагин пишет в свой scratch (TMPDIR, который выдаёт песочница) и читает файл
	// обратно. Каталог плагина намеренно read-only, поэтому маркер туда не пишем.
	script := "import os, sys\n" +
		"scratch = os.environ.get('TMPDIR') or '/tmp'\n" +
		"p = os.path.join(scratch, 'ran')\n" +
		"open(p, 'w').write('ran')\n" +
		"assert open(p).read() == 'ran', 'scratch не читается'\n" +
		"sys.stdout.write('{\"status\":\"ok\",\"output\":{\"scratch_writable\": true}}')\n"
	m := writePlugin(t, dir, script)

	res := ExecWithEnvCtx(AllowUntrustedPlugins(context.Background()), m, []byte("{}"), 30*time.Second, nil)
	if !res.OK() {
		t.Fatalf("untrusted-плагин в песочнице должен работать: %+v (stderr: %s)", res, res.Stderr)
	}
	if writable, _ := res.Output["scratch_writable"].(bool); !writable {
		t.Fatalf("песочница должна давать записываемый scratch: %+v", res.Output)
	}
}

func TestWindowsRefusalIsActionable(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		t.Skip("проверка только для платформ без изолятора")
	}
	m := &pipeline.Manifest{ID: "x", Runtime: pipeline.Runtime{Type: "python"}, Sandbox: pipeline.SandboxUntrusted, Dir: t.TempDir()}
	_, _, _, err := sandboxArgs(m, []string{"/bin/true"}, t.TempDir())
	if !errors.Is(err, ErrSandboxUnsupported) {
		t.Fatalf("ожидался ErrSandboxUnsupported, получено: %v", err)
	}
	// Оператор должен понимать, что флаг согласия проблему не решает.
	if !strings.Contains(err.Error(), "--allow-untrusted-plugins") {
		t.Errorf("сообщение должно предупреждать, что флаг не помогает: %v", err)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("сообщение должно называть платформу: %v", err)
	}
}

func TestUntrustedEnvExcludesUserProfile(t *testing.T) {
	// Профиль пользователя и произвольные переменные не должны попадать
	// в изолированный плагин.
	t.Setenv("WEDRA_TRUST_PROBE", "secret-value")
	env := untrustedBaseEnv(nil)
	if len(env) == 0 {
		t.Fatal("untrusted-окружение пустое — интерпретатор не запустится")
	}
	for _, kv := range env {
		for _, banned := range []string{"WEDRA_TRUST_PROBE=", "APPDATA=", "LOCALAPPDATA=", "PROGRAMDATA=", "PUBLIC="} {
			if strings.HasPrefix(kv, banned) {
				t.Fatalf("изолированный плагин получил %s", kv)
			}
		}
	}
}

func TestUntrustedEnvNeverCarriesSecrets(t *testing.T) {
	t.Setenv("WEDRA_TRUST_SECRET", "top-secret")
	m := &pipeline.Manifest{
		ID:          "s",
		Runtime:     pipeline.Runtime{Type: "python"},
		Sandbox:     pipeline.SandboxUntrusted,
		Permissions: pipeline.Permissions{Secrets: []string{"WEDRA_TRUST_SECRET"}},
	}
	for _, kv := range untrustedBaseEnv(nil) {
		if strings.HasPrefix(kv, "WEDRA_TRUST_SECRET=") {
			t.Fatalf("секрет попал в untrusted-окружение: %s", kv)
		}
	}
	// Валидатор не пропустит такое сочетание, но и exec-путь не должен доверять
	// только валидатору.
	if err := pipeline.ValidateManifest(m); err == nil {
		t.Fatal("untrusted + permissions.secrets должен отклоняться")
	}
}
