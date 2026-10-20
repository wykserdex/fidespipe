package api

import (
	"fmt"
	"os"
	"testing"

	"github.com/wykserdex/fidespipe/internal/testjob"
)

// TestMain вводит тестовый бинарь в Job Object с KILL_ON_JOB_CLOSE до старта
// любых детей.
//
// Пакет порождает процессы не прямо, а через core.Run → engine → шаги:
// TestAPIRunCancel поднимает настоящий ран со sleep-фикстурой (Python, спит
// 30 с) и отменяет его, gate_test.go гоняет пайплайны с гейтом. Ран стартует в
// горутине (server.go, POST /api/run), то есть его Python-потомок может
// пережить сам тест: тест возвращается по статусу «cancelled», а горутина с
// раном и её процесс живут дальше до отмены.
//
// Механизм: ребёнок без job наследует дескриптор stdout и сам не завершается,
// os/exec ждёт закрытия пайпа, EOF не наступает, команда не возвращает
// управление. Именно этот класс зависания и чинит KILL_ON_JOB_CLOSE — здесь он
// уместен как раз потому, что отмена рана идёт асинхронно.
//
// Обвязка — в internal/testjob, здесь только вызов: копия в каждом пакете
// разъехалась бы с оригиналом.
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
