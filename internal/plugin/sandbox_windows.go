//go:build windows

// Изолятор внешнего кода на Windows — AppContainer.
//
// Отличие от Linux не в деталях, а в самой модели. Там песочница — отдельная
// программа (bubblewrap), которой оборачивают запуск, и она может смонтировать
// файловую систему как угодно: «всё только на чтение» там выражается одним
// флагом. Здесь изоляция задаётся ПРАВАМИ САМОГО ПРОЦЕССА: у контейнера нет
// ни одной записи доступа, кроме явно выданных, и путь либо доступен, либо
// нет. Это плотнее, а не слабее, — но требует перечислить всё, что нужно
// выдать, включая каталог интерпретатора.
//
// Форма интеграции та же, что на Linux: launcher плюс аргументы. Launcher —
// это тот же бинарь WEDRA, но в скрытом режиме internal-sandbox-exec, который
// создаёт контейнеризованный процесс напрямую (см. internal/appcontainer и
// internal/cli/sandbox_exec.go). Обёрткой это не выражается: атрибуты
// PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES нельзя передать через os/exec.

package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wykserdex/wedra/internal/appcontainer"
	"github.com/wykserdex/wedra/internal/pipeline"
)

// winProbeOnce/winProbeOK — результат пробы хоста, один на процесс.
//
// Проба создаёт профиль контейнера, получает его SID и удаляет профиль.
// Никаких прав и процессов: этого достаточно, чтобы отличить «платформа
// умеет» от «политика хоста запрещает».
var (
	winProbeOnce sync.Once
	winProbeOK   bool
)

func sandboxBackend() (string, bool) { return "appcontainer", true }

// sandboxLauncher — тот же бинарь WEDRA: он же умеет в скрытом режиме
// создать контейнеризованный процесс.
func sandboxLauncher() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	return exe, true
}

// sandboxUsable — может ли хост создать контейнер.
//
// Наличия поддержки в Windows недостаточно: политика хоста может запретить
// создание профиля контейнера. Проба выполняется один раз на процесс и
// кэшируется.
func sandboxUsable() bool {
	winProbeOnce.Do(func() {
		winProbeOK = appcontainer.Probe() == nil
	})
	return winProbeOK
}

func sandboxArgs(m *pipeline.Manifest, argv []string, scratch string) (string, []string, sandboxNetwork, error) {
	launcher, ok := sandboxLauncher()
	if !ok {
		return "", nil, nil, fmt.Errorf("%w: не удалось определить путь к исполняемому файлу WEDRA — "+
			"запускать контейнеризованный процесс некому", ErrSandboxUnsupported)
	}
	if !sandboxUsable() {
		return "", nil, nil, fmt.Errorf("%w: хост не разрешил AppContainer — политика запрещает "+
			"создание профиля контейнера (проверьте политику AppContainer и право менять DACL)",
			ErrSandboxUnsupported)
	}
	// Сеть контейнеру без capability SID не выдаётся, а объявленная сеть —
	// это обещание плагину, которое нельзя выполнить молча. Отказ fail-closed:
	// плагин, которому нужна сеть, на Windows не запускается.
	if declaresNetwork(m) {
		return "", nil, nil, fmt.Errorf("%w: плагин объявил сетевой доступ, а изолятор на Windows "+
			"сеть контейнеру не выдаёт — запустить его здесь нельзя", ErrSandboxUnsupported)
	}
	args := []string{
		"internal-sandbox-exec",
		"--profile=" + appcontainer.ProfileName(m.ID),
		"--scratch=" + scratch,
		"--cwd=" + m.Dir,
	}
	for _, p := range sandboxReadPaths(m, argv) {
		args = append(args, "--read="+p)
	}
	args = append(args, "--")
	args = append(args, argv...)
	return launcher, args, nil, nil
}

// sandboxReadPaths — что контейнеру нужно видеть.
//
// Модель AppContainer: путь либо доступен, либо нет. Перечислить нужно всё, на
// что плагин смотрит: каталог плагина, каталог интерпретатора и каталог
// донора, если он объявлен. Всё это выдаётся ТОЛЬКО НА ЧТЕНИЕ и исполнение —
// права на запись не выдаются никогда, поэтому плагин не может переписать
// сам себя (на Linux каталог плагина монтируется на запись, и это отличие
// зафиксировано в SECURITY.md).
func sandboxReadPaths(m *pipeline.Manifest, argv []string) []string {
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		for _, existing := range out {
			if existing == abs {
				return
			}
		}
		out = append(out, abs)
	}
	add(m.Dir)
	if len(argv) > 0 {
		// Каталог интерпретатора (или самого бинарника для Go-плагинов).
		// Без этой выдачи контейнерный ребёнок создаётся, но не может загрузить
		// интерпретатор и выходит с кодом 5.
		add(filepath.Dir(argv[0]))
	}
	// Донор живёт вне каталога плагина (<плагины>/.donors/<имя>/) и путь к нему
	// приходит в окружении плагина. Без выдачи плагин с донором не доживёт
	// до первой команды.
	if v := donorEnvFor(m); v != "" {
		if i := strings.IndexByte(v, '='); i > 0 {
			add(donorRootFor(filepath.Dir(v[i+1:])))
		}
	}
	return out
}

// donorRootFor — каталог донора целиком: <плагины>/.donors/<имя>.
//
// Выдаётся не каталог бинарника, а корень донора: скрипт может звать соседние
// файлы из своего каталога, а выдача только одного файла такой случай
// пропустит.
func donorRootFor(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != ".donors" || i+1 >= len(parts) {
			continue
		}
		return filepath.FromSlash(strings.Join(parts[:i+2], "/"))
	}
	return path
}

// PrepareSandbox — готовит изолятор для плагина, установленного в каталог dir.
//
// Вынесено отдельно от запуска не для красоты: выдача прав контейнеру на
// каталог интерпретатора дорога. Windows пересчитывает наследование по всему
// дереву каталога при каждой записи, а под каталогом интерпретатора Python
// лежит 2,6 ГБ в site-packages: измерено 16-18 секунд на выдачу и столько же
// на повторную. Платить это на первом прогоне нельзя — человек ждёт запуска
// плагина, а не установки, — поэтому подготовка делается при установке.
//
// Ошибка here означает, что плагин в песочнице не запустится, но установку
// плагина она НЕ отменяет: плагин может быть доверенным, а решение о доверии
// принимает не установщик.
func PrepareSandbox(dir string) error {
	if dir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return fmt.Errorf("не читается plugin.yaml: %w", err)
	}
	var m pipeline.Manifest
	if err := pipeline.DecodeManifest(raw, &m); err != nil {
		return fmt.Errorf("некорректный манифест: %w", err)
	}
	m.Dir = dir
	return prepareSandbox(&m)
}

// prepareSandbox — выдаёт контейнеру доступ на каталог плагина, каталог
// интерпретатора и каталог донора.
//
// Тот же набор путей, что идёт в sandboxArgs: граница изоляции не должна
// различаться между подготовкой и запуском, иначе подготовка готовит одно, а
// запуск просит другое.
func prepareSandbox(m *pipeline.Manifest) error {
	if m == nil {
		return nil
	}
	argv, err := sandboxArgv(m)
	if err != nil {
		return err
	}
	return appcontainer.PrepareAccess(appcontainer.ProfileName(m.ID), sandboxReadPaths(m, argv))
}

// sandboxArgv — команда плагина в том виде, в каком она пойдёт в контейнер.
//
// Достаточно первого элемента: по нему определяется каталог интерпретатора.
// Для бинарного плагина это сам бинарник внутри каталога плагина, и отдельная
// выдача ему не нужна.
func sandboxArgv(m *pipeline.Manifest) ([]string, error) {
	switch m.Runtime.Type {
	case "python":
		py, err := pythonInterpreter()
		if err != nil {
			return nil, err
		}
		return []string{py}, nil
	case "binary":
		return []string{filepath.Join(m.Dir, m.Runtime.Entry)}, nil
	default:
		return []string{filepath.Join(m.Dir, m.Runtime.Entry)}, nil
	}
}
