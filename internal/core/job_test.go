package core

import (
	"fmt"
	"os"
	"testing"

	"github.com/wykserdex/fidespipe/internal/testjob"
)

// TestMain вводит тестовый бинарь в Job Object с KILL_ON_JOB_CLOSE до старта
// любых детей.
//
// Именно здесь это было нужнее всего: internal/core порождает процессы —
// exec-фикстуры (execPlugin → plugin.ExecWithEnvCtx), Run→engine→шаги, а под
// -tags integration ещё и Python на каждый шипленный плагин в
// TestPluginTestShippedPlugins. Зависание наблюдалось на этом пакете (22/22 за
// 387 с, затем висяк на том же коммите).
//
// Механизм воспроизведён и проверен: ребёнок без job наследует дескриптор
// stdout и сам не завершается, os/exec ждёт закрытия пайпа, EOF не наступает,
// команда не возвращает управление. Таймаут шага это не лечит.
//
// Обвязка — в internal/testjob, здесь только вызов: копия в каждом пакете
// разъехалась бы с оригиналом.
//
// Выполняется при ЛЮБОМ запуске тестов пакета, включая CI-шаг conformance
// (`go test -tags integration ./internal/core -run "TestPluginTest|TestExec"`)
// и шаг `fidespipe check`. Там он безвреден: job создаётся один раз за процесс и на
// логику тестов не влияет; наоборот, он гарантирует, что Python-потомки не
// переживут тестовый бинарь.
//
// Файл НЕ под //go:build windows, хотя механизм windows-only: так TestMain
// существует на всех платформах и на не-Windows зовёт no-op ветку testjob.
// Иначе набор TestMain'ов разъезжался бы по платформам.
func TestMain(m *testing.M) {
	if err := testjob.Adopt(); err != nil {
		fmt.Fprintf(os.Stderr, "testjob: %v — тесты оставили бы осиротевших детей\n", err)
		os.Exit(1)
	}
	if err := testjob.SelfTest(); err != nil {
		fmt.Fprintf(os.Stderr, "testjob: самопроверка не прошла: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	// Handle жив до самого os.Exit: именно его закрытие ядром и убивает job.
	testjob.KeepAlive()
	os.Exit(code)
}
