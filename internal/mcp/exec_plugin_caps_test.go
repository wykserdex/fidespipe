package mcp

// Строгое правило exec_plugin: инструмент идёт мимо пайплайна, а значит мимо
// human_gate, поэтому запускать через него можно только плагины без
// объявленных прав.
//
// Проверяется здесь и порядок, и содержание отказа. Порядок важен отдельно:
// отказ по правам должен наступать ДО чтения входа и запуска, иначе «мы
// ограничили инструмент» окажется ограничением, которое применяется после
// того, как код уже исполнен.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/plugin"
)

// capServer — сервер с реальным каталогом плагинов, чтобы проверка дошла до
// чтения манифеста: иначе отказ был бы отказом по пути, а не по правам.
func capServer(t *testing.T, manifest map[string]interface{}) *Server {
	t.Helper()
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	dir := filepath.Join(plugins, "cap")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Выход плагина обязан соответствовать протоколу, иначе проверка контракта
	// выхода откажет и тест измерял бы не то.
	script := "import sys,json\nsys.stdin.read()\nsys.stdout.write(json.dumps({'status':'ok','output':{}}))\n"
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	s := agentServer()
	s.workDir = root
	s.multi = &multiEngine{dirs: []string{plugins}, workDir: root}
	// Доверие выдано явно, как это сделал бы оператор: тестовый плагин не в
	// allow-list, и без этого проверка прав не наступала бы — отказало бы
	// доверие. Так тест проверяет ровно то, что задумано: у ДОВЕРЕННОГО
	// плагина с объявленными правами exec_plugin всё равно отказывает.
	s.trust.Trusted = trustDirsIn(t, plugins)
	// Каталог ранов нужен даже безопасному пути: след запуска пишется ДО кода, и
	// без него отказ был бы про журнал, а не про права.
	runs := filepath.Join(root, "runs")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		t.Fatal(err)
	}
	s.runsDir = runs
	return s
}

func baseManifest() map[string]interface{} {
	return map[string]interface{}{
		"id": "cap", "version": "0.1.0", "platform_api": "0.1",
		"runtime": map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":   map[string]interface{}{},
		"output":  map[string]interface{}{},
	}
}

func withPerms(m map[string]interface{}, perms map[string]interface{}) map[string]interface{} {
	m["permissions"] = perms
	return m
}

// rpcCode достаёт машинный код отказа. Отказ без читаемого кода — сам по себе
// дефект: агенту нужен код, по которому он может отличить «нельзя» от
// «не получилось», а не разбирать текст.
func rpcCode(t *testing.T, e *RPCError) string {
	t.Helper()
	data, ok := e.Data.(map[string]string)
	if !ok {
		t.Fatalf("отказ без машинного кода в Data (получено %#v): %s", e.Data, e.Message)
	}
	return data["code"]
}

func assertCapRefusal(t *testing.T, s *Server, want string) {
	t.Helper()
	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": "cap"})
	if rpcErr == nil {
		t.Fatal("плагин с объявленными правами нельзя запускать через exec_plugin")
	}
	if got := rpcCode(t, rpcErr); got != want {
		t.Errorf("код отказа = %q, ожидался %q (сообщение: %s)", got, want, rpcErr.Message)
	}
	// Подсказку про human_gate требуем только у своего кода: отказ по сети
	// появился раньше и объясняется по-своему, приписывать ему чужую подсказку
	// незачем — тест проверяет чужое поведение и не должен его переписывать.
	if want == "E_AGENT_EXEC_CAPABILITIES" && !strings.Contains(rpcErr.Message, "human_gate") {
		t.Errorf("отказ должен называть выход (run_pipeline с human_gate), получено: %s", rpcErr.Message)
	}
}

// Каждая способность по отдельности: правило не должно ловить «всё сразу» и
// молча пропускать одну из них.
func TestToolExecPluginRefusesDeclaredCapabilities(t *testing.T) {
	cases := []struct {
		name  string
		perms map[string]interface{}
	}{
		{"секреты", map[string]interface{}{"secrets": []interface{}{"TOKEN"}}},
		{"запись на диск", map[string]interface{}{"filesystem": "workspace"}},
		{"чтение и запись", map[string]interface{}{"filesystem": "readwrite"}},
		{"сеть", map[string]interface{}{"network": []interface{}{map[string]interface{}{"any_host": true}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Сеть ловится раньше, отдельным кодом: так она была и до этого
			// правила, и менять её код незачем.
			want := "E_AGENT_EXEC_CAPABILITIES"
			if tc.name == "сеть" {
				want = "E_NETWORK_DENIED"
			}
			assertCapRefusal(t, capServer(t, withPerms(baseManifest(), tc.perms)), want)
		})
	}
}

// filesystem: read — право только на чтение, гейт его не требовал и здесь
// требовать нечего. Если бы правило было слишком широким, плагин перестал бы
// запускаться вообще, и exec_plugin стал бы мёртвым инструментом.
func TestToolExecPluginAllowsSafeCapabilities(t *testing.T) {
	s := capServer(t, withPerms(baseManifest(), map[string]interface{}{
		"network": []interface{}{}, "filesystem": "read", "secrets": []interface{}{},
	}))
	out, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": "cap"})
	if rpcErr != nil {
		t.Fatalf("безопасный плагин должен запускаться, отказ: %s", rpcErr.Message)
	}
	if !strings.Contains(out, "output") {
		t.Errorf("ожидался JSON с выходом плагина, получено: %s", out)
	}
}

// Манифест без объявления прав — не то же самое, что «прав нет»: правило
// считает незнание опасным. Плагин, чей манифест не прочитался, запускаться
// не должен.
func TestToolExecPluginRefusesUnreadableManifest(t *testing.T) {
	s := agentServer()
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	// Ни plugin.yaml, ни ничего: плагина нет, и LoadManifest обязан это поймать
	// раньше, чем дойдёт дело до прав. Отказ ожидаем любой, но он обязан быть.
	s.workDir = root
	s.multi = &multiEngine{dirs: []string{plugins}, workDir: root}
	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": "missing"})
	if rpcErr == nil {
		t.Error("плагин без манифеста не должен запускаться")
	}
}

// Отказ по правам обязан наступать ДО аудита намерения: иначе в журнале
// появилась бы запись о запуске, которого не было. Проверяем, что след не
// записан.
func TestToolExecPluginCapabilityRefusalWritesNoIntent(t *testing.T) {
	runs := t.TempDir()
	s := capServer(t, withPerms(baseManifest(), map[string]interface{}{
		"secrets": []interface{}{"TOKEN"},
	}))
	s.runsDir = runs
	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": "cap"})
	if rpcErr == nil {
		t.Fatal("ожидался отказ по правам")
	}
	if _, err := os.Stat(filepath.Join(runs, agentExecAuditFile)); !os.IsNotExist(err) {
		t.Errorf("отказ по правам не должен оставлять след запуска, получено: %v", err)
	}
}

// При двух отказах побеждает более фундаментальный. Плагин агента, который
// ещё и объявляет права, недоверен И имеет capabilities: отказ должен называть
// недоверенность, потому что подсказка «запусти через run_pipeline с гейтом»
// на хосте без изолятора была бы неверной — там такой плагин не запустится
// вообще, никаким способом.
func TestToolExecPluginTrustRefusalWinsOverCapabilities(t *testing.T) {
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	dir := filepath.Join(plugins, plugin.AgentPluginDir, "cap")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := withPerms(baseManifest(), map[string]interface{}{"secrets": []interface{}{"TOKEN"}})
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Выход плагина обязан соответствовать протоколу, иначе проверка контракта
	// выхода откажет и тест измерял бы не то.
	script := "import sys,json\nsys.stdin.read()\nsys.stdout.write(json.dumps({'status':'ok','output':{}}))\n"
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	s := agentServer()
	s.workDir = root
	s.runsDir = filepath.Join(root, "runs")
	s.multi = &multiEngine{dirs: []string{plugins}, workDir: root}
	s.trust.AllowUntrusted = false

	_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": dir})
	if rpcErr == nil {
		t.Fatal("ожидался отказ")
	}
	if got := rpcCode(t, rpcErr); got != "E_AGENT_PLUGIN_UNTRUSTED" {
		t.Errorf("код отказа = %q, ожидался E_AGENT_PLUGIN_UNTRUSTED (сообщение: %s)", got, rpcErr.Message)
	}
}

// Неверный timeout_seconds — явная ошибка, а не тихое значение по умолчанию.
// Раньше timeout_seconds=1000, 0, -5 или строка молча превращались в 60, и
// агент думал, что получил запрошенное время.
func TestToolExecPluginRejectsBadTimeout(t *testing.T) {
	for _, v := range []interface{}{1000.0, 0.0, -5.0, "60s", true} {
		s := capServer(t, withPerms(baseManifest(), map[string]interface{}{
			"network": []interface{}{}, "filesystem": "read", "secrets": []interface{}{},
		}))
		_, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": "cap", "timeout_seconds": v})
		if rpcErr == nil {
			t.Errorf("timeout_seconds=%v: ожидалась ошибка, а не запуск", v)
			continue
		}
		if !strings.Contains(rpcErr.Message, "timeout_seconds") {
			t.Errorf("timeout_seconds=%v: ошибка должна называть поле: %s", v, rpcErr.Message)
		}
	}
}

// Дробные секунды принимаются точно: 0.5с — это полсекунды, а не ноль
// (усечение давало бы мгновенный таймаут).
func TestToolExecPluginAcceptsFractionalTimeout(t *testing.T) {
	s := capServer(t, withPerms(baseManifest(), map[string]interface{}{
		"network": []interface{}{}, "filesystem": "read", "secrets": []interface{}{},
	}))
	out, _, rpcErr := s.toolExecPlugin(nil, map[string]interface{}{"plugin": "cap", "timeout_seconds": 0.5})
	if rpcErr != nil {
		t.Fatalf("timeout_seconds=0.5 должен приниматься, отказ: %s", rpcErr.Message)
	}
	if !strings.Contains(out, "output") {
		t.Errorf("ожидался выход плагина, получено: %s", out)
	}
}
