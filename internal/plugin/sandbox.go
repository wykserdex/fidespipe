//lint:file-ignore SA4023 Проверка «сравнение всегда истинно» срабатывает здесь
// только на платформе без изолятора: sandboxArgs на ней отказывает всегда, и
// `if err != nil` в sandboxCommand выглядит тождественно истинным. В сборке с
// изолятором (Linux) nil-ошибка — штатный путь, и проверка обязательна. Подавлено
// на уровне файла, а не строки: директива //lint:ignore обязана что-то подавлять
// и на Linux ругается «didn't match anything», то есть роняет CI на зелёном коде.

package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// ErrSandboxUnsupported — на этой платформе/хосте нет рабочей изоляции.
// Fail-closed: ядро предпочитает отказать в запуске, чем выполнить untrusted-плагин
// без песочницы.
var ErrSandboxUnsupported = errors.New("os-изоляция внешнего кода недоступна")

// declaresNetwork — песочница даёт сеть только под явный any_host: true.
//
// Раньше здесь стояло len(Permissions.Network) > 0, и это была дыра: плагин с
// заявлением «нужен api.telegram.org:443» получал весь интернет, потому что
// структурированный список схлопывался в булев флаг, и намерение автора
// молча игнорировалось. Точечный фильтр по host:port не реализован ни на
// одной платформе (bwrap не умеет, sandbox-exec умеет только allow/deny),
// поэтому исполнимым является лишь blanket-вариант — и он должен быть
// написан явно. Плагины со списком хостов сеть не получают вовсе; их
// отсекает runner с внятным объяснением.
func declaresNetwork(m *pipeline.Manifest) bool {
	return pipeline.NetworkRequestsAnyHost(m)
}

// sandboxEgress — поднимает egress-фильтр для плагина, которому объявлен
// any_host, и отдаёт переменные окружения с адресом прокси.
//
// Fail-closed: если прокси поднять не удалось, плагин с сетью не запускается
// вовсе. Молча запустить без фильтра хуже, чем отказать.
func sandboxEgress(ctx context.Context, m *pipeline.Manifest) ([]string, func(), error) {
	noop := func() {}
	eg, err := newEgress(m.Permissions.Network)
	if err != nil {
		return nil, noop, fmt.Errorf("egress-фильтр: %w", err)
	}
	stop := eg.stop
	// Прокси живёт в неизолированном процессе Fidespipe, поэтому его нужно гасить
	// вместе с контекстом запуска, иначе порт и сокеты переживут плагин.
	go func() {
		<-ctx.Done()
		eg.stop()
	}()
	return eg.env(), stop, nil
}

// sandboxNetwork — платформенная работа с сетью песочницы, которая нужна
// ПОСЛЕ старта процесса: поднять egress и только потом выпустить плагин.
//
// nil означает «ничего делать не надо» — либо сеть не объявлена, либо платформе
// нечего делать.
type sandboxNetwork interface {
	// attach вызывается сразу после cmd.Start() и до ожидания завершения.
	// Возвращает release, который обязан быть вызван; при ошибке процесс
	// плагина не запускается (fail-closed).
	attach(cmd *exec.Cmd) (release func(), err error)
}

// sandboxCommand — обёртка для запуска внешнего кода. Launcher и его аргументы
// собираются платформенной реализацией sandboxArgs (bubblewrap на Linux; на
// macOS бэкенд отключён и вынесен из дерева в историю git, поэтому изолятора
// там нет),
// и процесс не создаётся вовсе.
func sandboxCommand(ctx context.Context, argv []string, m *pipeline.Manifest, scratch string) (*exec.Cmd, sandboxNetwork, error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("пустая команда плагина")
	}
	launcher, args, net, err := sandboxArgs(m, argv, scratch)
	if err != nil {
		return nil, nil, err
	}
	return exec.CommandContext(ctx, launcher, args...), net, nil
}

// newSandboxScratch — приватный записываемый каталог на один запуск плагина.
// Вызывающий обязан вызвать cleanup после завершения процесса.
//
// Собственный каталог вместо общего os.TempDir(): он приватен этому запуску и
// гарантированно существует до монтирования. Точка монтирования, которой нет на
// хосте, не годится: после --ro-bind / / создать её уже нельзя, и bwrap падает с
// "Read-only file system".
func newSandboxScratch() (string, func(), error) {
	noop := func() {}
	dir, err := os.MkdirTemp("", "fidespipe-sbx-")
	if err != nil {
		return "", noop, fmt.Errorf("scratch для песочницы: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// sandboxScratchEnv — HOME/TMPDIR, которые получают оба бэкенда. На Linux bwrap
// пробрасывает окружение, на macOS профиль открывает запись только для scratch, —
// без этих переменных плагин на macOS писал бы в read-only каталог хоста.
// Путь резолвится, чтобы совпадать с тем, что попало в правила песочницы.
func sandboxScratchEnv(scratch string) []string {
	dir := resolveSandboxPath(scratch)
	return []string{"HOME=" + dir, "TMPDIR=" + dir, "TEMP=" + dir, "TMP=" + dir}
}

// sandboxBackendName — человекочитаемое имя изолятора (для логов и ошибок).
func sandboxBackendName() string {
	if name, ok := sandboxBackend(); ok {
		return name
	}
	return "нет"
}

// SandboxUsable — есть ли на этой платформе рабочий изолятор.
//
// Наружу, потому что интерфейс должен объяснять человеку, ПОЧЕМУ плагин
// агента не запустится, а не просто не показывать его. Ответ «плагина нет»
// и ответ «плагин есть, но на этой ОС изолятора нет» — разные сообщения, и
// второе полезнее.
//
// BackendName для сообщения: на Linux bwrap, на остальных платформах «нет».
func SandboxUsable() bool {
	_, ok := sandboxBackend()
	return ok
}

// SandboxBackendName — имя изолятора или «нет», если его на этой платформе
// не существует.
func SandboxBackendName() string { return sandboxBackendName() }

// resolveSandboxPath — путь для правил песочницы. Обязательно резолвим symlink'и:
// на macOS $TMPDIR лежит под /var -> /private/var, а sandbox-exec матчится по
// уже резолвнутому пути, из-за чего нерезолвнутый путь не попадает в allow-правило
// и запись честного плагина запрещается.
func resolveSandboxPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
