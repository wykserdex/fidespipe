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
// это тот же бинарь Fidespipe, но в скрытом режиме internal-sandbox-exec, который
// создаёт контейнеризованный процесс напрямую (см. internal/appcontainer и
// internal/cli/sandbox_exec.go). Обёрткой это не выражается: атрибуты
// PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES нельзя передать через os/exec.

package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wykserdex/fidespipe/internal/appcontainer"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/sandboxexec"
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

// sandboxLauncher — тот же бинарь Fidespipe: он же умеет в скрытом режиме
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
		return "", nil, nil, fmt.Errorf("%w: не удалось определить путь к исполняемому файлу Fidespipe — "+
			"запускать контейнеризованный процесс некому", ErrSandboxUnsupported)
	}
	if !sandboxUsable() {
		return "", nil, nil, fmt.Errorf("%w: хост не разрешил AppContainer — политика запрещает "+
			"создание профиля контейнера (проверьте политику AppContainer и право менять DACL)",
			ErrSandboxUnsupported)
	}
	// Сеть контейнеру выдаётся только через capability internetClient
	// (S-1-15-3-1). Capability работает не везде: системе нужно знать, что
	// такая capability за что отвечает, а на части машин таблицы capability
	// нет вовсе, и тогда capability в токене не даёт ничего.
	//
	// Объявленная сеть — это обещание плагину, и выполнить его молча нельзя.
	// Поэтому сперва ПРОБА: если capability на этой машине не действует, плагин
	// с сетью не запускается вовсе, с внятным объяснением. Молчаливый запуск
	// без сети выглядел бы как «плагин сломался», и чинить его стали бы в
	// плагине, а не в песочнице.
	if declaresNetwork(m) {
		ok, detail, err := networkCapabilityWorks()
		if err != nil {
			return "", nil, nil, fmt.Errorf("%w: плагин объявил сетевой доступ, а проверить "+
				"capability не удалось: %v — запустить его здесь нельзя", ErrSandboxUnsupported, err)
		}
		if !ok {
			return "", nil, nil, fmt.Errorf("%w: плагин объявил сетевой доступ, а capability "+
				"internetClient на этой машине не действует (проба: %s) — недоверенный плагин "+
				"с сетью на Windows запустить нельзя; запустите его как доверенный (по хэшу) "+
				"или перенесите цепочку на Linux", ErrSandboxUnsupported, detail)
		}
	}
	args := []string{
		"internal-sandbox-exec",
		"--profile=" + appcontainer.ProfileName(m.ID),
		"--scratch=" + scratch,
		"--cwd=" + m.Dir,
	}
	if declaresNetwork(m) {
		args = append(args, "--network")
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

// networkCapabilityWorks — даёт ли capability internetClient сеть контейнеру.
//
// Проба выполняется один раз на процесс: она поднимает контейнер, а это
// сотни миллисекунд, и ответ на вопрос «работает ли capability на этой
// машине» не меняется.
var (
	netProbeOnce sync.Once
	netProbeOK   bool
	netProbeText string
	netProbeErr  error
)

func networkCapabilityWorks() (bool, string, error) {
	netProbeOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			netProbeErr = fmt.Errorf("не найден исполняемый файл Fidespipe: %w", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		netProbeOK, netProbeText, netProbeErr = sandboxexec.ProbeNetwork(ctx, exe)
	})
	return netProbeOK, netProbeText, netProbeErr
}

// SandboxNetworkNote — строка о сети контейнера для отчёта doctor.
//
// На других платформах пустая: там сеть выдаётся иначе (на Linux — через
// userspace-стек и прокси), и спрашивать про capability там нечего.
func SandboxNetworkNote() string {
	ok, detail, err := networkCapabilityWorks()
	switch {
	case err != nil:
		return "сеть контейнеру: НЕ ПРОВЕРЕНА (" + err.Error() + ")"
	case ok:
		return "сеть контейнеру: ДОСТУПНА (capability internetClient действует)"
	default:
		return "сеть контейнеру: НЕДОСТУПНА (" + detail + ")"
	}
}
