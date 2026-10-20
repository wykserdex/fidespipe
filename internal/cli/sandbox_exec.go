package cli

// Скрытая подкоманда: запуск внешнего кода в контейнере AppContainer.
//
// Сама работа помощника лежит в internal/sandboxexec: её должен уметь
// запускать и тестовый бинарь internal/plugin, а тот не может импортировать
// internal/cli — тот импортирует plugin. Здесь только входная точка.
//
// Помощник вызывается из internal/plugin/sandbox_windows.go, а не человеком:
// в справке он не значится, и его аргументы — контракт между ядром и
// помощником, а не интерфейс для пользователя.

import (
	"os"

	"github.com/wykserdex/wedra/internal/sandboxexec"
)

// handleSandboxExec — точка входа скрытой подкоманды.
func handleSandboxExec(args []string) {
	os.Exit(sandboxexec.Run(args))
}
