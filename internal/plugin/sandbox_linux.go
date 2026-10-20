//go:build linux

package plugin

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// Linux backend: bubblewrap. Требует непривилегированных user namespaces.
//
// Модель: файловая система хоста доступна только на чтение (`--ro-bind / /`),
// каталог плагина тоже read-only — плагин не может себя модифицировать и закрепиться
// на диске. PID/IPC/UTS-пространства отдельные, /tmp — свежий tmpfs, HOME
// перенаправлен в /tmp.
//
// СЕТЬ ИЗОЛИРОВАНА ВСЕГДА (--unshare-net), независимо от declarations.
// Раньше netns добавлялся только когда плагин сеть не объявлял, и объявленный
// any_host получал сетевой namespace ХОСТА целиком. Это была дыра шире, чем
// «нефильтрованный egress»: недоверенный плагин доставал сервисы хоста на
// 127.0.0.1, включая собственный HTTP API WEDRA.
//
// Плагину с any_host выход наружу даёт userspace-сетевой стек (slirp4netns):
// tap0 внутри netns песочницы, трафик NAT-ится на хосте. Измерено: хостовый
// loopback изнутри не виден (--disable-host-loopback), а интернет и DNS есть.
//
// Чего это НЕ даёт: фильтрации по destination. Плагин — root в своём userns и
// удаляет собственные правила nftables, поэтому in-netns фильтр не контроль.
// Per-destination фильтр для недоверенного кода на Linux требует привилегий на
// хосте. Подробности и замеры — в SECURITY.md.

const (
	slirpTapName = "tap0"
	slirpMTU     = "65520"
	// slirpGuestDNS — адрес DNS-форвардера userspace-стека в гостевой сети.
	slirpGuestDNS = "10.0.2.3"
	// netGateName — файл-воротце в scratch. Плагин не стартует, пока файл не
	// появится: иначе он успевает позвонить до того, как tap0 поднят.
	netGateName   = ".wedra-net-gate"
	netResolvName = ".wedra-resolv.conf"
	// netAttachTimeout — сколько ждём pid в новом netns и tap0. Проба хоста
	// упала на RTM_NEWADDR; здесь падать нечему, кроме отсутствия tap0.
	netAttachTimeout = 10 * time.Second
)

var (
	linuxProbeOnce sync.Once
	linuxProbeOK   bool
)

func sandboxBackend() (string, bool) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return "", false
	}
	return "bwrap", true
}

func sandboxLauncher() (string, bool) {
	p, err := exec.LookPath("bwrap")
	if err != nil {
		return "", false
	}
	return p, true
}

// sandboxUsable — может ли хост реально создать песочницу. Наличия bwrap в PATH
// недостаточно: user namespaces могут быть запрещены политикой ядра или
// ограничением CI-раннера (bwrap падает на RTM_NEWADDR при --unshare-net).
// Проба выполняется один раз на процесс и кэшируется.
func sandboxUsable() bool {
	launcher, ok := sandboxLauncher()
	if !ok {
		return false
	}
	linuxProbeOnce.Do(func() {
		c := exec.Command(launcher, "--unshare-all", "--ro-bind", "/", "/",
			"--proc", "/proc", "--dev", "/dev", "/bin/true")
		c.Stdout, c.Stderr = io.Discard, io.Discard
		linuxProbeOK = c.Run() == nil
	})
	return linuxProbeOK
}

func sandboxArgs(m *pipeline.Manifest, argv []string, scratch string) (string, []string, sandboxNetwork, error) {
	launcher, ok := sandboxLauncher()
	if !ok {
		return "", nil, nil, fmt.Errorf("%w: bwrap (bubblewrap) не найден в PATH — установите пакет bubblewrap", ErrSandboxUnsupported)
	}
	if !sandboxUsable() {
		return "", nil, nil, fmt.Errorf("%w: bwrap есть, но хост не разрешает user namespaces (--unshare-all) — песочницу собрать нельзя", ErrSandboxUnsupported)
	}
	net, err := sandboxNetworkFor(m, scratch)
	if err != nil {
		return "", nil, nil, err
	}
	return launcher, sandboxArgsUnchecked(m, argv, scratch), net, nil
}

// linuxNetSetup — egress через slirp4netns для плагина, объявившего any_host.
type linuxNetSetup struct {
	slirp     string
	scratch   string
	gate      string
	resolvSrc string
}

func (n *linuxNetSetup) attach(cmd *exec.Cmd) (func(), error) {
	if cmd == nil || cmd.Process == nil {
		return nil, fmt.Errorf("%w: процесс песочницы не запущен — сеть поднимать некому", ErrSandboxUnsupported)
	}
	// pid ищем сами: внутренний $$ плагина при --unshare-pid — это pid внутри
	// нового PID namespace, для slirp4netns он бесполезен. Нужен host-pid
	// потомка bwrap, и обязательно с ДРУГИМ netns: попытка цепить slirp к самому
	// bwrap добавила tap0 в netns хоста (проверено), то есть сломала бы хост.
	pid, err := waitNetnsPID(cmd.Process.Pid, netAttachTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: не нашёл процесс плагина в отдельном сетевом namespace: %v", ErrSandboxUnsupported, err)
	}
	var logBuf cappedWriter4
	sl := exec.Command(n.slirp, "--configure", "--mtu="+slirpMTU, "--disable-host-loopback", strconv.Itoa(pid), slirpTapName)
	sl.Stdout, sl.Stderr = &logBuf, &logBuf
	if err := sl.Start(); err != nil {
		return nil, fmt.Errorf("%w: не запустился %s: %v", ErrSandboxUnsupported, filepath.Base(n.slirp), err)
	}
	stop := func() {
		_ = sl.Process.Kill()
		_ = sl.Wait()
	}
	if err := waitTapReady(pid, netAttachTimeout); err != nil {
		stop()
		return nil, fmt.Errorf("%w: %s не поднял интерфейс %s в сети песочницы: %v (%s)",
			ErrSandboxUnsupported, filepath.Base(n.slirp), slirpTapName, err, strings.TrimSpace(logBuf.String()))
	}
	// Только теперь выпускаем плагин.
	if err := os.WriteFile(n.gate, []byte("go\n"), 0o600); err != nil {
		stop()
		return nil, fmt.Errorf("%w: не открыл ворота запуска плагина: %v", ErrSandboxUnsupported, err)
	}
	return stop, nil
}

// cappedWriter4 — сбор вывода slirp4netns с потолком, чтобы его нельзя было
// залить память процесса WEDRA.
type cappedWriter4 struct {
	buf  bytes.Buffer
	cap  int
	seen bool
}

func (w *cappedWriter4) Write(p []byte) (int, error) {
	if w.cap == 0 {
		w.cap = 8 << 10
	}
	if w.buf.Len() < w.cap {
		w.buf.Write(p)
	} else {
		w.seen = true
	}
	return len(p), nil
}

func (w *cappedWriter4) String() string {
	if w.seen {
		return w.buf.String() + "…"
	}
	return w.buf.String()
}

// sandboxNetworkFor — что нужно с сетью для этого манифеста.
//
// Сеть не объявлена → nil (песочница просто без выхода, slirp не нужен).
// Объявлена → нужен slirp4netns; нет бинаря → fail-closed, потому что молча
// запустить плагин без запрошенной сети хуже, чем отказать.
func sandboxNetworkFor(m *pipeline.Manifest, scratch string) (sandboxNetwork, error) {
	if !declaresNetwork(m) {
		return nil, nil
	}
	slirp, err := exec.LookPath("slirp4netns")
	if err != nil {
		return nil, fmt.Errorf("%w: плагин %s объявил any_host: true, но slirp4netns не найден в PATH — "+
			"сеть недоверенному плагину на Linux даётся только через него, а общий с хостом namespace "+
			"больше не используется (установите пакет slirp4netns или уберите сеть из манифеста)",
			ErrSandboxUnsupported, m.ID)
	}
	dir := resolveSandboxPath(scratch)
	// Резолвер: в песочнице нужен адрес userspace-стека, а не хостовый. Хостовый
	// /etc/resolv.conf внутри изолированного netns неотвечаем — на Debian он
	// указывает на адрес вне песочницы, и любой DNS-запрос истекает по таймауту.
	resolvSrc := filepath.Join(dir, netResolvName)
	if err := os.WriteFile(resolvSrc, []byte("nameserver "+slirpGuestDNS+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("%w: не подготовлен резолвер для песочницы: %v", ErrSandboxUnsupported, err)
	}
	return &linuxNetSetup{
		slirp:     slirp,
		scratch:   dir,
		gate:      filepath.Join(dir, netGateName),
		resolvSrc: resolvSrc,
	}, nil
}

// sandboxArgsUnchecked — чистая сборка аргументов bwrap без проверки capability
// хоста. Вынесена отдельно, чтобы форма команды тестировалась даже там, где
// раннер запрещает user namespaces и sandboxArgs вернёт отказ.
func sandboxArgsUnchecked(m *pipeline.Manifest, argv []string, scratch string) []string {
	dir := resolveSandboxPath(scratch)
	args := []string{
		"--die-with-parent",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup-try",
		// Сеть изолируется ВСЕГДА. Объявление any_host больше не означает общий
		// с хостом namespace: выход наружу даёт userspace-стек (см. attach).
		"--unshare-net",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		// Единственная точка записи: приватный каталог этого запуска (HOME/TMPDIR
		// пробрасываются через cmd.Env). Каталог плагина остаётся read-only, /tmp
		// хоста — тоже, поэтому подложить файл в чужой временный каталог нельзя.
		"--bind", dir, dir,
		"--chdir", resolveSandboxPath(m.Dir),
	}
	if declaresNetwork(m) {
		// Резолвер переопределяем на адрес userspace-стека. Биндим по
		// разрешённому пути: /etc/resolv.conf на хосте — симлинок, и bwrap не
		// умеет создавать файл поверх симлинка ("Can't create file").
		args = append(args, "--bind", filepath.Join(dir, netResolvName), hostResolvTarget())
		args = append(args, "--")
		// Ворота: плагин не стартует, пока WEDRA не поднимет tap0. Скрипт
		// передаётся отдельным аргументом: слитая в одну строку команда была бы
		// для bwrap именем программы ("execvp ...: No such file or directory").
		args = append(args, "/bin/sh", "-c", netGateScript, "wedra-netgate", filepath.Join(dir, netGateName))
		args = append(args, argv...)
		return args
	}
	args = append(args, "--")
	args = append(args, argv...)
	return args
}

// netGateScript — ожидание воротца. $1 — путь к файлу-воротцу, $2.. — команда
// плагина (после shift она становится $@). exec обязателен: иначе запустился бы
// сам sh, а не плагин.
const netGateScript = `while [ ! -e "$1" ]; do sleep 0.05; done; shift; exec "$@"`

// hostResolvTarget — куда биндить свой резолвер, чтобы это увидел плагин.
// Если /etc/resolv.conf симлинк (WSL, systemd-resolved), биндим по разрешённому
// пути: bwrap не может создать файл поверх симлинка.
func hostResolvTarget() string {
	const p = "/etc/resolv.conf"
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// waitNetnsPID — host-pid процесса bwrap, находящегося в netns, отличном от
// нашего. Читаем /proc/<pid>/task/*/children: bwrap форкает ребёнка, который и
// держит новые namespace, а сам остаётся в родительском netns.
//
// Проверка netns обязательна и вторая проверка тоже: цепление slirp к процессу
// в нашем netns добавит tap0 в netns ХОСТА.
func waitNetnsPID(parent int, timeout time.Duration) (int, error) {
	own, err := netnsOf(os.Getpid())
	if err != nil {
		return 0, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, c := range childrenOf(parent) {
			ns, err := netnsOf(c)
			if err != nil || ns == "" || ns == own {
				continue
			}
			return c, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, fmt.Errorf("pid %d не породил потомка в отдельном netns за %s", parent, timeout)
}

func childrenOf(pid int) []int {
	matches, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range matches {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(raw)) {
			if n, err := strconv.Atoi(field); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

func netnsOf(pid int) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid))
}

// waitTapReady — ждём появления tap0 в /proc/<pid>/net/dev. Файл net-устройств
// принадлежит конкретному netns, поэтому это проверка именно песочницы, а не
// хоста: если интерфейс появился у хоста, значит slirp цепляется не туда.
func waitTapReady(pid int, timeout time.Duration) error {
	devPath := fmt.Sprintf("/proc/%d/net/dev", pid)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(devPath)
		if err == nil && strings.Contains(string(raw), " "+slirpTapName+":") {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("интерфейс %s не появился в /proc/%d/net/dev за %s", slirpTapName, pid, timeout)
}

// PrepareSandbox — на Linux делать нечего.
//
// bubblewrap собирает песочницу на каждый запуск, и никаких прав на диске
// выдавать не нужно: изоляция задаётся mount-пространством процесса.
func PrepareSandbox(dir string) error { return nil }
