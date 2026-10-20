//go:build !linux && !windows

//lint:file-ignore U1000,SA4023 Платформенный файл: на этой платформе sandboxArgs
// всегда отказывает, поэтому проверки «сравнение всегда истинно» и «функция не
// используется» — артефакты того, что линтер разбирает платформенную сборку.
// В сборке с изолятором (Linux) обе функции живут в sandbox_linux.go, и там
// sandboxArgs возвращает nil-ошибку штатно.

package plugin

import (
	"fmt"
	"runtime"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// macOS и прочие платформы, кроме Linux и Windows: поддерживаемого изолятора нет.
// Fail-closed — untrusted-код на таких хостах не запускается, а не запускается
// без песочницы.
//
// Windows вынесен в отдельный файл (sandbox_windows.go) с бэкендом AppContainer:
// изоляция там задаётся правами самого процесса, а не обёрткой, и выражается
// иначе — через скрытый режим того же бинаря. Описание модели и её отличие от
// Linux — в SECURITY.md, раздел «An AppContainer backend for Windows».
//
// Job Objects — НЕ часть этой ступени и не граница безопасности:
// JobObjectSecurityLimitInformation удалён начиная с Windows Vista,
// JOB_OBJECT_SET_SECURITY_ATTRIBUTES не поддерживается. Job objects дают
// лимиты CPU, памяти и числа процессов и гарантированное завершение дерева
// процессов. Защиту от DoS и уборку — да, изоляцию — нет. На Windows, где
// изолятор есть, Job Objects работают вместе с ним: контейнеризованный ребёнок
// наследует джобу родителя, и её закрытие гасит всё дерево.
//
// macOS попадает сюда намеренно. Реализация на sandbox-exec была, но
// sandbox-exec не умеет read-only bind mount, поэтому каталог плагина приходилось
// открывать на запись — и плагин мог переписать сам себя и закрепиться на диске.
// Защита целостности на macOS была неполной, поэтому бэкенд вместе с
// поведенческим тестом вынесен из дерева в историю git (коммит 9d17120).
// Вернуть можно, но сначала нужно решить, чем закрыть эту дыру.

func sandboxBackend() (string, bool) { return "", false }

func sandboxLauncher() (string, bool) { return "", false }

func sandboxUsable() bool { return false }

func sandboxArgs(m *pipeline.Manifest, argv []string, scratch string) (string, []string, sandboxNetwork, error) {
	// Сообщение намеренно говорит, что --allow-untrusted-plugins не поможет:
	// оператор не должен искать решение в флагах, когда изолятора нет вовсе.
	return "", nil, nil, fmt.Errorf("%w: на %s изоляция внешнего кода не реализована "+
		"(нужен bwrap на Linux; бэкенд для macOS отключён — см. SECURITY.md); "+
		"флаг --allow-untrusted-plugins не обходит песочницу, поэтому внешний код "+
		"на этой платформе запустить нельзя",
		ErrSandboxUnsupported, runtime.GOOS)
}

// PrepareSandbox — на платформах без изолятора делать нечего.
//
// Изолятора нет, и плагин без песочницы не запустится вовсе (fail-closed), —
// готовить нечего.
func PrepareSandbox(dir string) error { return nil }

// SandboxNetworkNote — пустая строка: на этой платформе изолятора нет, и спрашивать про capability нечего.
func SandboxNetworkNote() string { return "" }
