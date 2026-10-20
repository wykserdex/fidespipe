package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/wykserdex/fidespipe/internal/testjob"
)

// TestMain вводит тестовый бинарь в Job Object с KILL_ON_JOB_CLOSE до старта
// любых детей.
//
// Пакет порождает процессы постоянно, и почти все они долгоживущие:
// census-примитивы в check_test.go запускают sleeper (cmd /c exit,
// powershell Start-Sleep) и ждут его смерти, TestWaitProcessLimitThenKillTree
// и TestRepeatedAwaitDoesNotCallWaitTwice — процессы на 60 и 120 секунд,
// которые убиваются через killTree (taskkill /T /F).
//
// Механизм: ребёнок без job наследует дескриптор stdout и сам не завершается,
// os/exec ждёт закрытия пайпа, EOF не наступает, команда не возвращает
// управление. Для этого пакета он особенно болезнен: тесты сами проверяют
// killTree и пределы ожидания, то есть работают ровно с тем классом зависаний,
// который обвязка и закрывает. Если бы такой тест завис, он повис бы на
// внешнем таймауте пакета.
//
// Обвязка — в internal/testjob, здесь только вызов.
//
// Файл НЕ под //go:build windows, хотя механизм windows-only: так TestMain
// существует на всех платформах и на не-Windows зовёт no-op ветку testjob.
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
