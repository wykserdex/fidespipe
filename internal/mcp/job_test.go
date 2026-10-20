package mcp

import (
	"fmt"
	"os"
	"testing"

	"github.com/wykserdex/fidespipe/internal/testjob"
)

// TestMain вводит тестовый бинарь в Job Object с KILL_ON_JOB_CLOSE до старта
// любых детей.
//
// Пакет порождает Python-процессы: инструмент exec_plugin зовёт plugin.Exec
// напрямую, минуя пайплайн, а sleeperServer в agent_exec_audit_test.go
// поднимает настоящий плагин, который спит заданное число секунд — так
// измеряется, состоялся ли запуск. Тесты отмечены testing.Short() и при
// отсутствии интерпретатора пропускаются, но когда python есть, процессы
// настоящие.
//
// Механизм: ребёнок без job наследует дескриптор stdout и сам не
// завершается, os/exec ждёт закрытия пайпа, EOF не наступает, команда не
// возвращает управление. Таймаут это не лечит — процесс надо убить, а его
// нечем.
//
// Обвязка — в internal/testjob, здесь только вызов: копия в каждом пакете
// разъехалась бы с оригиналом.
//
// Файл НЕ под //go:build windows, хотя механизм windows-only: так TestMain
// существует на всех платформах и на не-Windows зовёт no-op ветку testjob.
// Иначе набор TestMain-ов разъезжался бы по платформам.
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
