package plugin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// requirePython — тесты с реальным запуском процесса. Пробный запуск, а не
// только поиск в PATH: на Windows "python" может быть App Execution Alias
// (заглушка), который находится, но не запускает интерпретатор.
func requirePython(t *testing.T) {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if out, err := exec.Command(p, "-c", "import sys; sys.stdout.write('ok')").Output(); err == nil && strings.TrimSpace(string(out)) == "ok" {
			return
		}
	}
	t.Skip("работающий интерпретатор python3/python не найден в окружении")
}

// trustManifest — минимальный python-плагин, создающий маркер при запуске.
func trustManifest(t *testing.T, sandbox string) (*Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := "import sys\n" +
		"open(" + quotePy(marker) + ", 'w').write('ran')\n" +
		"sys.stdout.write('{\"status\":\"ok\",\"output\":{}}')\n"
	entry := filepath.Join(dir, "plugin.py")
	if err := os.WriteFile(entry, []byte(script), 0o600); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	m := &pipeline.Manifest{
		ID:      "trust-test",
		Version: "0.1.0",
		Runtime: pipeline.Runtime{Type: "python", Entry: "plugin.py"},
		Sandbox: sandbox,
		Dir:     dir,
	}
	return m, marker
}

// quotePy — путь в исходнике python-литералом (бэкслеши Windows экранируются).
func quotePy(s string) string { return "'" + strings.ReplaceAll(s, `\`, `\\`) + "'" }

func TestUntrustedManifestRefusedWithoutIsolation(t *testing.T) {
	requirePython(t)
	m, marker := trustManifest(t, pipeline.SandboxUntrusted)
	res := Exec(m, []byte("{}"), 5*time.Second)

	if res.OK() {
		t.Fatalf("untrusted-плагин не должен запускаться без изоляции: %+v", res)
	}
	if res.ErrCode != "sandbox_unavailable" {
		t.Fatalf("ErrCode = %q, ожидался sandbox_unavailable", res.ErrCode)
	}
	if !res.Platform {
		t.Fatal("отказ по доверию должен быть платформенной ошибкой (Platform=true)")
	}
	if res.ExitCode != 2 {
		t.Fatalf("ExitCode = %d, ожидался 2", res.ExitCode)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("процесс плагина был запущен — маркер создан")
	}
}

// TestDenyUntrustedPolicyBlocksEverything
func TestDenyUntrustedPolicyBlocksEverything(t *testing.T) {
	requirePython(t)
	m, marker := trustManifest(t, "")
	ctx := WithTrustPolicy(context.Background(), TrustPolicy{DenyUntrusted: true})
	res := ExecWithEnvCtx(ctx, m, []byte("{}"), 5*time.Second, nil)

	if res.OK() || res.ErrCode != "sandbox_unavailable" {
		t.Fatalf("политика --deny-untrusted-plugins обязана блокировать запуск: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("процесс запущен при DenyUntrusted — маркер создан")
	}
}

// Плагин, которому ядро выдало доверие (allow-list по хэшу), работает как
// раньше: с правами пользователя, без изолятора.
//
// Это ОБРАТНАЯ сторона инверсии, и она тоже обязана быть закреплена тестом:
// иначе « доверен = всегда в песочнице» прошло бы незамеченным, и официальные
// плагины потеряли бы доступ к файлам и сети без всякой видимой причины.
func TestAllowedPluginRunsWithoutSandbox(t *testing.T) {
	requirePython(t)
	m, marker := trustManifest(t, "")
	res := ExecWithEnvCtx(trustCtx(t, m), m, []byte("{}"), 20*time.Second, nil)
	if !res.OK() {
		t.Fatalf("доверенный плагин должен работать как раньше: %+v (stderr: %s)", res, res.Stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("маркер не создан: %v", err)
	}
}

func TestUntrustedRefusedBeforeRuntimeLookup(t *testing.T) {
	// Гейт срабатывает до поиска интерпретатора: даже битый runtime не
	// маскирует отказ по политике доверия.
	m := &Manifest{
		ID:      "no-runtime",
		Runtime: pipeline.Runtime{Type: "python", Entry: "missing.py"},
		Sandbox: pipeline.SandboxUntrusted,
		Dir:     t.TempDir(),
	}
	res := Exec(m, []byte("{}"), time.Second)
	if res.ErrCode != "sandbox_unavailable" {
		t.Fatalf("ErrCode = %q, ожидался sandbox_unavailable", res.ErrCode)
	}
}

func TestAllowUntrustedPolicyIsExplicit(t *testing.T) {
	// AllowUntrusted — явный вызов в trusted-коде; политика из ctx
	// читается корректно и не влияет на доверенные манифесты.
	m := &Manifest{ID: "x", Runtime: pipeline.Runtime{Type: "python"}, Sandbox: pipeline.SandboxUntrusted}
	if denied := enforceTrust(m, TrustPolicyFrom(context.Background())); denied == nil {
		t.Fatal("пустая политика обязана быть fail-closed")
	}
	if denied := enforceTrust(m, TrustPolicyFrom(AllowUntrustedPlugins(context.Background()))); denied != nil {
		t.Fatalf("AllowUntrusted должен снимать отказ: %+v", denied)
	}
	if denied := enforceTrust(&Manifest{ID: "y"}, TrustPolicy{DenyUntrusted: true}); denied == nil {
		t.Fatal("DenyUntrusted должен блокировать любой плагин")
	}
	// `sandbox: trusted` в манифесте НЕ является разрешением на запуск. Раньше
	// именно это поле было единственным источником доверия, поэтому проверка
	// обязана остаться: иначе возврат к модели «плагин сам себя объявляет
	// доверенным» прошёл бы незамеченным.
	selfTrusted := &Manifest{ID: "y", Sandbox: pipeline.SandboxTrusted, Dir: t.TempDir()}
	if denied := enforceTrust(selfTrusted, TrustPolicy{}); denied == nil {
		t.Fatal("sandbox: trusted в манифесте не должен давать доверия без записи в allow-list")
	}
	// А вот с выданным ядром доверием — должен.
	trusted := NewAllowList()
	digest, err := ContentDigest(selfTrusted.Dir)
	if err != nil {
		t.Fatal(err)
	}
	trusted.Allow(selfTrusted.ID, digest)
	if denied := enforceTrust(selfTrusted, TrustPolicy{Trusted: trusted}); denied != nil {
		t.Fatalf("плагин из allow-list должен идти в exec: %+v", denied)
	}
}
