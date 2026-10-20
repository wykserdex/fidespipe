package mcp

import (
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/plugin"
)

// agentServer — сервер с ЯВНО разрешённым exec_plugin. Тесты ниже проверяют
// разбор аргументов и границы пути, а не политику, поэтому они должны
// разрешать запуск: иначе их отличает отказ по политике, а проверяемое
// поведение не наступает.
func agentServer() *Server {
	return &Server{
		trust:   plugin.TrustPolicy{AgentCanExec: true},
		execSem: make(chan struct{}, MaxConcurrentAgentExec),
	}
}

// Политика доверия по умолчанию закрывает инструмент. Проверка разрешения
// идёт ПЕРВОЙ, до аргументов, до чтения манифеста и до запуска: без явного
// согласия ядра агент не исполняет код вообще.
func TestToolExecPluginDeniedByDefault(t *testing.T) {
	s := &Server{}
	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": "echo_ok"})
	if rpcErr == nil {
		t.Fatal("exec_plugin без политики должен отказать, а не запускать код")
	}
	if !strings.Contains(rpcErr.Message, "--allow-agent-exec") {
		t.Error("отказ должен называть нужный флаг, получено: " + rpcErr.Message)
	}
	// Отказ до разбора аргументов: даже корректный аргумент не проходит.
	_, _, rpcErr = s.toolExecPlugin(nil, map[string]interface{}{})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "--allow-agent-exec") {
		t.Error("проверка разрешения должна идти до проверки аргументов, получено: " + rpcErr.Message)
	}
}

func TestToolExecPluginRequiresPlugin(t *testing.T) {
	s := agentServer()
	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{})
	if rpcErr == nil {
		// Fatal, а не Error: следующая строка читает rpcErr.Message, и при
		// nil-ошибке тест падал бы паникой вместо отчёта.
		t.Fatal("exec_plugin без plugin должен вернуть ошибку")
	}
	if !strings.Contains(rpcErr.Message, "нужен plugin") {
		t.Error("Ожидается сообщение 'нужен plugin', получено: " + rpcErr.Message)
	}
}

func TestToolExecPluginRejectsOutsideRoot(t *testing.T) {
	s := agentServer()
	s.workDir = "/tmp/test"
	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{
		"plugin": "../outside",
	})
	if rpcErr == nil {
		t.Fatal("exec_plugin с .. должен вернуть ошибку")
	}
	if !strings.Contains(rpcErr.Message, "E_PLUGIN_OUTSIDE_ROOT") {
		t.Error("Ожидается E_PLUGIN_OUTSIDE_ROOT, получено: " + rpcErr.Message)
	}
}

func TestToolExecPluginRejectsUnknownPlugin(t *testing.T) {
	s := agentServer()
	s.workDir = "/tmp/test"
	s.multi = &multiEngine{dirs: []string{"/tmp/test/plugins"}, workDir: "/tmp/test"}
	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{
		"plugin": "nonexistent/plugin",
	})
	if rpcErr == nil {
		t.Error("exec_plugin с неизвестным плагином должен вернуть ошибку")
	}
}

// Потолок параллельных запусков — единственное, что не даёт агенту занять
// сервер: вызов синхронный и живёт до maxAgentExecTimeout. Проверяем, что при
// исчерпанных слотах новый вызов ОТКАЗЫВАЕТСЯ, а не встаёт в очередь.
func TestToolExecPluginRespectsConcurrencyLimit(t *testing.T) {
	s := agentServer()
	s.workDir = "/tmp/test"
	s.multi = &multiEngine{dirs: []string{"/tmp/test/plugins"}, workDir: "/tmp/test"}

	// Занимаем все слоты, как будто параллельные вызовы уже идут.
	for i := 0; i < MaxConcurrentAgentExec; i++ {
		if err := s.acquireExecSlot(); err != nil {
			t.Fatalf("слот %d не выдался: %v", i, err.Message)
		}
	}
	defer func() {
		for i := 0; i < MaxConcurrentAgentExec; i++ {
			s.releaseExecSlot()
		}
	}()

	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": "echo_ok"})
	if rpcErr == nil {
		t.Fatal("при исчерпанных слотах вызов должен отказать, а не выполняться")
	}
	if !strings.Contains(rpcErr.Message, "предел параллельных запусков") {
		t.Error("отказ должен называть предел, получено: " + rpcErr.Message)
	}

	// Слот освободился — вызов снова проходит дальше по проверкам.
	s.releaseExecSlot()
	if err := s.acquireExecSlot(); err != nil {
		t.Fatalf("после освобождения слот должен выдаться: %v", err.Message)
	}
}

// Ноль инициализированных семафоров — это «сервер собран не через NewServer».
// Отказываем, а не выполняем: предел, которого нет, равен отсутствию предела.
func TestToolExecPluginRefusesWithoutSlots(t *testing.T) {
	s := &Server{trust: plugin.TrustPolicy{AgentCanExec: true}}
	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": "echo_ok"})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "не инициализированы") {
		t.Errorf("без слотов вызов должен отказать, получено: %v", rpcErr)
	}
}
