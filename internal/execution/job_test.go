package execution

import (
	"fmt"
	"os"
	"testing"

	"github.com/wykserdex/fidespipe/internal/testjob"
)

// TestMain вводит тестовый бинарь в Job Object с KILL_ON_JOB_CLOSE до старта
// любых детей.
//
// Пакет гоняет пайплайны целиком: Run() → engine → шаги, то есть порождает
// Python-процесс на каждый запуск. cancel_test.go запускает sleeper-фикстуру
// и отменяет ран через контекст, проверяя, что группа умирает быстро и статус
// cancelled, а не timeout. Это ровно тот случай, где процесс мог бы остаться
// жив и подвесить шаг.
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
