//go:build linux

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// Аргументы bwrap проверяются без запуска: на CI-раннерах user namespaces могут
// быть запрещены, и behavioural-тест песочницы там пропускается — форма команды
// всё равно обязана быть верной.

func TestLinuxSandboxArgs(t *testing.T) {
	dir := t.TempDir()
	m := &pipeline.Manifest{
		ID:      "x",
		Runtime: pipeline.Runtime{Type: "python", Entry: "plugin.py"},
		Dir:     dir,
	}
	scratch := t.TempDir()
	args := sandboxArgsUnchecked(m, []string{"/usr/bin/python3", "/p/plugin.py"}, scratch)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--die-with-parent",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--ro-bind / /",
		"--proc /proc",
		"--bind " + resolveSandboxPath(scratch) + " " + resolveSandboxPath(scratch),
		"--unshare-net",
		"-- /usr/bin/python3 /p/plugin.py",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("в аргументах bwrap нет %q: %s", want, joined)
		}
	}
	// Регрессия: перекрытие /tmp скрывало бы каталог плагина, установленного
	// во временный каталог, и процесс не смог бы стартовать. Точка монтирования
	// также должна существовать заранее: после --ro-bind / / создать её нельзя.
	if strings.Contains(joined, "--tmpfs") {
		t.Errorf("песочница не должна использовать tmpfs: %s", joined)
	}
	if _, err := os.Stat(resolveSandboxPath(scratch)); err != nil {
		t.Fatalf("scratch-каталог должен существовать до монтирования: %v", err)
	}
	// HOME/TMPDIR идут через окружение процесса, а не через --setenv: один
	// источник правды для обоих бэкендов.
	if strings.Contains(joined, "--setenv") {
		t.Errorf("HOME/TMPDIR должны приходить из cmd.Env, а не из --setenv: %s", joined)
	}
	if env := sandboxScratchEnv(scratch); !strings.Contains(strings.Join(env, ","), "TMPDIR="+scratch) {
		t.Errorf("scratch-окружение не содержит TMPDIR: %v", env)
	}
	// Каталог плагина не должен пробрасываться на запись.
	if strings.Contains(joined, "--bind "+resolveSandboxPath(dir)) {
		t.Errorf("каталог плагина не должен быть rw: %s", joined)
	}
}

// TestLinuxSandboxBlocksPluginDirWrite — security-регрессия: каталог плагина
// должен быть read-only, иначе внешний код может модифицировать себя и
// закрепиться на диске между запусками.
func TestLinuxSandboxBlocksPluginDirWrite(t *testing.T) {
	if !sandboxUsable() {
		t.Skip("песочница недоступна на этом хосте")
	}
	requirePython(t)
	dir := t.TempDir()
	script := "import os, sys\n" +
		"here = os.path.dirname(os.path.abspath(__file__))\n" +
		"try:\n" +
		"    open(os.path.join(here, 'selfmod.py'), 'w').write('x')\n" +
		"except OSError:\n" +
		"    sys.stdout.write('{\"status\":\"ok\",\"output\":{\"plugin_dir_readonly\": true}}')\n" +
		"else:\n" +
		"    sys.exit(1)\n"
	m := writePlugin(t, dir, script)

	res := ExecWithEnvCtx(AllowUntrustedPlugins(context.Background()), m, []byte("{}"), 30*time.Second, nil)
	if !res.OK() {
		t.Fatalf("плагин не должен получать запись в свой каталог: %+v (stderr: %s)", res, res.Stderr)
	}
	if ro, _ := res.Output["plugin_dir_readonly"].(bool); !ro {
		t.Fatal("песочница не заблокировала запись в каталог плагина")
	}
	if _, err := os.Stat(filepath.Join(dir, "selfmod.py")); err == nil {
		t.Fatal("плагин смог создать файл в своём каталоге на хосте")
	}
}

// Сеть изолируется ВСЕГДА. Это регрессия на дыру шире, чем «нефильтрованный
// egress»: при объявленном any_host песочница раньше делила netns с хостом, и
// недоверенный плагин доставал сервисы хоста на 127.0.0.1, включая HTTP API
// Fidespipe. Теперь netns отдельный всегда, а выход наружу (если объявлен) даёт
// userspace-стек — см. attach.
func TestLinuxSandboxAlwaysIsolatesNetwork(t *testing.T) {
	cases := []struct {
		name    string
		net     []pipeline.NetworkPermission
		wantNet bool // ожидается ли egress-запуск (воротца и подмена резолвера)
	}{
		{"сеть не объявлена", nil, false},
		{"список host:port неисполним", []pipeline.NetworkPermission{{Host: "api.example.com", Port: 443}}, false},
		{"явный any_host", []pipeline.NetworkPermission{{AnyHost: true}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &pipeline.Manifest{
				ID:          "x",
				Runtime:     pipeline.Runtime{Type: "python", Entry: "plugin.py"},
				Dir:         t.TempDir(),
				Permissions: pipeline.Permissions{Network: tc.net},
			}
			scratch := t.TempDir()
			joined := strings.Join(sandboxArgsUnchecked(m, []string{"/usr/bin/python3", "/p/plugin.py"}, scratch), " ")

			if !strings.Contains(joined, "--unshare-net") {
				t.Error("netns должен быть отдельным всегда, включая объявленный any_host")
			}
			gated := strings.Contains(joined, netGateScript)
			if gated != tc.wantNet {
				t.Errorf("воротца запуска: получено %v, ожидалось %v: %s", gated, tc.wantNet, joined)
			}
			// Подмена резолвера нужна ровно тогда, когда есть egress: хостовый
			// resolv.conf внутри изолированного netns неотвечаем.
			hasResolv := strings.Contains(joined, "--bind "+resolveSandboxPath(scratch)+string(filepath.Separator)+netResolvName)
			if hasResolv != tc.wantNet {
				t.Errorf("подмена резолвера: получено %v, ожидалось %v: %s", hasResolv, tc.wantNet, joined)
			}
			// Команда плагина обязана остаться последней: ворота её запускают.
			if !strings.HasSuffix(joined, "-- /usr/bin/python3 /p/plugin.py") &&
				!strings.HasSuffix(joined, "/usr/bin/python3 /p/plugin.py") {
				t.Errorf("команда плагина должна быть последней: %s", joined)
			}
		})
	}
}

// Резолвер биндится по разрешённому пути: /etc/resolv.conf на хосте обычно
// симлинк, а bwrap не умеет создавать файл поверх симлинка.
func TestLinuxSandboxBindsResolvedResolvPath(t *testing.T) {
	if hostResolvTarget() == "/etc/resolv.conf" {
		t.Skip("на этом хосте /etc/resolv.conf — обычный файл, проверять нечего")
	}
	m := &pipeline.Manifest{
		ID:          "x",
		Runtime:     pipeline.Runtime{Type: "python", Entry: "plugin.py"},
		Dir:         t.TempDir(),
		Permissions: pipeline.Permissions{Network: []pipeline.NetworkPermission{{AnyHost: true}}},
	}
	joined := strings.Join(sandboxArgsUnchecked(m, []string{"/usr/bin/python3", "/p/plugin.py"}, t.TempDir()), " ")
	if !strings.Contains(joined, " "+hostResolvTarget()) {
		t.Errorf("резолвер должен биндиться по разрешённому пути %s: %s", hostResolvTarget(), joined)
	}
}

// Fail-closed: плагин с any_host без slirp4netns не запускается. Молча отдать
// ему песочницу без сети значило бы выполнить внешний код вопреки его
// манифесту, а вернуть общий netns с хостом — вернуть ту дыру, что закрыта.
func TestLinuxNetworkFailsClosedWithoutSlirp(t *testing.T) {
	m := &pipeline.Manifest{
		ID:          "x",
		Runtime:     pipeline.Runtime{Type: "python", Entry: "plugin.py"},
		Dir:         t.TempDir(),
		Permissions: pipeline.Permissions{Network: []pipeline.NetworkPermission{{AnyHost: true}}},
	}
	if _, err := exec.LookPath("slirp4netns"); err == nil {
		t.Skip("slirp4netns есть в PATH — проверяется поведенческим тестом")
	}
	t.Setenv("PATH", t.TempDir())
	_, err := sandboxNetworkFor(m, t.TempDir())
	if !errors.Is(err, ErrSandboxUnsupported) {
		t.Fatalf("без slirp4netns ожидался ErrSandboxUnsupported, получено: %v", err)
	}
	if !strings.Contains(err.Error(), "slirp4netns") {
		t.Errorf("отказ должен называть причину (slirp4netns): %v", err)
	}
}

// Сеть не объявлена — slirp не нужен вовсе, иначе мы бы требовали лишнюю
// зависимость для плагинов, которым сеть не даётся.
func TestLinuxNetworkNotRequiredWithoutDeclaration(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // slirp4netns заведомо недоступен
	m := &pipeline.Manifest{
		ID:      "x",
		Runtime: pipeline.Runtime{Type: "python", Entry: "plugin.py"},
		Dir:     t.TempDir(),
	}
	net, err := sandboxNetworkFor(m, t.TempDir())
	if err != nil || net != nil {
		t.Fatalf("без объявления сети slirp не требуется: net=%v err=%v", net, err)
	}
}

// childrenOf / netnsOf / waitTapReady — вспомогательная разведка /proc. Здесь
// проверяется то, что можно проверить без запуска песочницы: собственный pid
// виден, его netns читается, а дети bwrap перечисляются.
func TestLinuxProcIntrospection(t *testing.T) {
	ns, err := netnsOf(os.Getpid())
	if err != nil || ns == "" {
		t.Fatalf("netns собственного процесса не читается: %q %v", ns, err)
	}
	if kids := childrenOf(os.Getpid()); kids == nil {
		// nil допустим: у процесса без детей файла children может не быть.
		t.Log("детей нет — это нормально")
	}
	if _, err := netnsOf(-1); err == nil {
		t.Error("несуществующий pid не должен давать netns")
	}
}

// TestLinuxSandboxIsolatesHostLoopbackForNetworkPlugin — поведенческая проверка
// главного изменения. Плагин объявляет any_host и получает egress, но при этом
// НЕ должен доставать сервисы хоста на loopback: раньше он делил netns с хостом.
//
// Требует bwrap, python и slirp4netns — на CI-раннерах без них тест пропускается,
// поэтому поведение проверяется ещё и вручную (см. замеры в SECURITY.md).
func TestLinuxSandboxIsolatesHostLoopbackForNetworkPlugin(t *testing.T) {
	if !sandboxUsable() {
		t.Skip("песочница недоступна на этом хосте")
	}
	if _, err := exec.LookPath("slirp4netns"); err != nil {
		t.Skip("slirp4netns не установлен — сетевой путь песочницы не проверяем")
	}
	requirePython(t)

	// Слушаем на loopback хоста: именно это плагин раньше доставал.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("не удалось занять порт на loopback хоста: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	dir := t.TempDir()
	script := fmt.Sprintf(`import socket, sys, json
out = {}
def probe(host, port):
    try:
        s = socket.create_connection((host, port), timeout=4)
        s.close()
        return "reachable"
    except OSError as e:
        return "blocked:" + type(e).__name__
out["host_loopback"] = probe("127.0.0.1", %s)
sys.stdout.write(json.dumps({"status": "ok", "output": out}))
`, port)
	m := writePlugin(t, dir, script)
	m.Permissions.Network = []pipeline.NetworkPermission{{AnyHost: true}}

	res := ExecWithEnvCtx(AllowUntrustedPlugins(context.Background()), m, []byte("{}"), 60*time.Second, nil)
	if !res.OK() {
		t.Fatalf("плагин с any_host должен запускаться при наличии slirp4netns: %+v (stderr: %s)", res, res.Stderr)
	}
	got, _ := res.Output["host_loopback"].(string)
	if !strings.HasPrefix(got, "blocked") {
		t.Fatalf("песочница не изолировала loopback хоста: %v", got)
	}
}
