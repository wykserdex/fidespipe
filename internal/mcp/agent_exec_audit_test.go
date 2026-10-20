package mcp

// Тесты аудита exec_plugin.
//
// Этот код — журнал безопасности: он фиксирует, что агент запускал код.
// Контракт изменился один раз: запись переехала ДО запуска, и на один
// запуск теперь приходится две строки — намерение и результат.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

func auditTestServer(t *testing.T, runsDir string) *Server {
	t.Helper()
	return &Server{
		trust:   plugin.TrustPolicy{AgentCanExec: true},
		execSem: make(chan struct{}, MaxConcurrentAgentExec),
		runsDir: runsDir,
	}
}

func auditManifest(id string) *pipeline.Manifest {
	return &pipeline.Manifest{
		ID:      id,
		Version: "0.1.0",
		Dir:     filepath.Join("plugins", id),
	}
}

// readAudit читает журнал как список записей. Порядок сохраняется: он и есть
// смысл журнала.
func readAudit(t *testing.T, dir string) []map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, agentExecAuditFile))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("строка аудита не разбирается как JSON: %v\n%s", err, line)
		}
		out = append(out, rec)
	}
	return out
}

func str(rec map[string]interface{}, key string) string {
	s, _ := rec[key].(string)
	return s
}

// writeIntent пишет намерение и возвращает его id. Общий шаг для тестов.
func writeIntent(t *testing.T, s *Server, ref string, m *pipeline.Manifest) string {
	t.Helper()
	id, err := s.auditAgentExecIntent(ref, m, 60)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// rpcCodeOf достаёт машинный код из RPCError. rpcErr кладёт его в
// Data["code"], а не в Message, поэтому проверять надо именно Data.
func rpcCodeOf(e *RPCError) string {
	if e == nil {
		return ""
	}
	if m, ok := e.Data.(map[string]string); ok {
		return m["code"]
	}
	return ""
}

func TestAgentExecAuditIntentHasNoResultYet(t *testing.T) {
	dir := t.TempDir()
	s := auditTestServer(t, dir)
	// Доверенный плагин. После инверсии (H1) в журнал пишется реальный вердикт
	// ядра, а не «не написан агентом»: проверять untrusted=false на плагине,
	// которому никто не выдавал доверия, было бы проверкой противоположного.
	fxDir := writeFakePluginDir(t, t.TempDir(), "text_stats", echoerScript)
	writeIntent(t, s, "text_stats", trustManifest(t, s, fxDir))

	recs := readAudit(t, dir)
	if len(recs) != 1 {
		t.Fatalf("записей %d, ждали 1 (одно намерение)", len(recs))
	}
	got := recs[0]
	if str(got, "event") != agentExecEventIntent {
		t.Errorf("event = %q, ждали %q", str(got, "event"), agentExecEventIntent)
	}
	for k, want := range map[string]interface{}{
		"source":    "agent_auto",
		"plugin":    "text_stats",
		"plugin_id": "text_stats",
		"untrusted": false,
		"timeout":   float64(60),
	} {
		if got[k] != want {
			t.Errorf("intent[%q] = %v, ждали %v", k, got[k], want)
		}
	}
	// Исхода у намерения быть не может: плагин ещё не запускался.
	for _, k := range []string{"exit_code", "duration", "err_code"} {
		if _, ok := got[k]; ok {
			t.Errorf("в намерении не должно быть поля %q", k)
		}
	}
	if str(got, "exec_id") == "" {
		t.Error("у намерения должен быть exec_id, иначе записи не сойтись")
	}
	if str(got, "ts") == "" {
		t.Error("у намерения должна быть метка времени")
	}
}

// Намерение и результат связаны одним exec_id, и в журнале они идут именно в
// таком порядке.
func TestAgentExecAuditResultFollowsIntentWithSameExecID(t *testing.T) {
	dir := t.TempDir()
	s := auditTestServer(t, dir)
	m := auditManifest("text_stats")

	execID := writeIntent(t, s, "text_stats", m)
	if err := s.auditAgentExecResult(execID, "text_stats", m,
		&plugin.ExecResult{ExitCode: 3, ErrCode: "boom"}, 250*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	recs := readAudit(t, dir)
	if len(recs) != 2 {
		t.Fatalf("записей %d, ждали 2 (намерение + результат)", len(recs))
	}
	if str(recs[0], "event") != agentExecEventIntent {
		t.Errorf("первая запись = %q, ждали намерение", str(recs[0], "event"))
	}
	if str(recs[1], "event") != agentExecEventResult {
		t.Errorf("вторая запись = %q, ждали результат", str(recs[1], "event"))
	}
	if str(recs[0], "exec_id") != execID || str(recs[1], "exec_id") != execID {
		t.Errorf("exec_id не совпал: %q / %q (ждали %q)",
			str(recs[0], "exec_id"), str(recs[1], "exec_id"), execID)
	}
	if str(recs[0], "ts") > str(recs[1], "ts") {
		t.Error("метка времени намерения позже результата")
	}
	if recs[1]["exit_code"] != float64(3) || str(recs[1], "err_code") != "boom" {
		t.Errorf("результат не перенёс исход: %+v", recs[1])
	}
	if d, err := time.ParseDuration(str(recs[1], "duration")); err != nil || d != 250*time.Millisecond {
		t.Errorf("duration = %q", str(recs[1], "duration"))
	}
}

// Append-only: три запуска — шесть строк, ничего не перезаписано.
func TestAgentExecAuditAppendsBothRecordsPerRun(t *testing.T) {
	dir := t.TempDir()
	s := auditTestServer(t, dir)

	for _, ref := range []string{"first", "second", "third"} {
		m := auditManifest(ref)
		id := writeIntent(t, s, ref, m)
		if err := s.auditAgentExecResult(id, ref, m, &plugin.ExecResult{}, 0); err != nil {
			t.Fatal(err)
		}
	}
	recs := readAudit(t, dir)
	if len(recs) != 6 {
		t.Fatalf("записей %d, ждали 6", len(recs))
	}
	seen := map[string]int{}
	for _, r := range recs {
		seen[str(r, "plugin")]++
	}
	for _, want := range []string{"first", "second", "third"} {
		if seen[want] != 2 {
			t.Errorf("плагин %q: записей %d, ждали 2 (намерение + результат)", want, seen[want])
		}
	}
}

func TestAgentExecAuditMarksUntrusted(t *testing.T) {
	dir := t.TempDir()
	s := auditTestServer(t, dir)

	// Первый — плагин агента: внешний код по построению.
	agent := &pipeline.Manifest{ID: "mailer", Version: "0.1.0",
		Dir: filepath.Join("work", "agent-plugins", "mailer")}
	// Второй — доверенный плагин: ядро выдало доверие по хэшу содержимого.
	plain := trustManifest(t, s, writeFakePluginDir(t, t.TempDir(), "echoer", echoerScript))
	writeIntent(t, s, "agent-plugins/mailer", agent)
	writeIntent(t, s, plain.ID, plain)

	recs := readAudit(t, dir)
	if len(recs) != 2 {
		t.Fatalf("записей %d, ждали 2", len(recs))
	}
	for i, want := range []bool{true, false} {
		if recs[i]["untrusted"] != want {
			t.Errorf("запись %d: untrusted = %v, ждали %v", i+1, recs[i]["untrusted"], want)
		}
	}
}

func TestAgentExecAuditFailsWithoutRunsDir(t *testing.T) {
	s := auditTestServer(t, "")
	if _, err := s.auditAgentExecIntent("x", auditManifest("x"), 60); err == nil {
		t.Fatal("без каталога ранов аудит обязан падать: иначе запуск неоплачен")
	}
}

// Помеха выбрана так, чтобы работать и на Unix, и на Windows: на месте
// каталога кладётся файл, а не выставляются права — на Windows права
// выставляются иначе и такой тест там проходил бы вхолостую.
func TestAgentExecAuditFailsWhenRunsDirIsNotADir(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := auditTestServer(t, blocker)
	_, err := s.auditAgentExecIntent("x", auditManifest("x"), 60)
	if err == nil {
		t.Fatal("аудит обязан падать, если каталог недоступен")
	}
	if !strings.Contains(err.Error(), agentExecAuditFile) {
		t.Errorf("в ошибке должно называться имя файла журнала: %v", err)
	}
}

func TestAgentExecAuditFailsWhenJournalPathIsADir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, agentExecAuditFile), 0o755); err != nil {
		t.Fatal(err)
	}
	s := auditTestServer(t, dir)
	if _, err := s.auditAgentExecIntent("x", auditManifest("x"), 60); err == nil {
		t.Fatal("аудит обязан падать, если на месте журнала каталог")
	}
	if err := s.auditAgentExecResult("id", "x", auditManifest("x"), &plugin.ExecResult{}, 0); err == nil {
		t.Fatal("запись результата обязана падать на тех же условиях")
	}
}

// Потерянный результат не стирает намерение. Это и есть смысл двух записей:
// плагин уже отработал, и в журнале остаётся хотя бы след попытки.
func TestAgentExecAuditResultFailureLeavesIntent(t *testing.T) {
	good := t.TempDir()
	s := auditTestServer(t, good)
	m := auditManifest("text_stats")
	execID := writeIntent(t, s, "text_stats", m)

	// Тот же запуск, но запись результата уходит в недоступный каталог.
	broken := auditTestServer(t, filepath.Join(t.TempDir(), "missing"))
	if err := broken.auditAgentExecResult(execID, "text_stats", m, &plugin.ExecResult{}, 0); err == nil {
		t.Fatal("запись результата в недоступный каталог обязана падать")
	}

	recs := readAudit(t, good)
	if len(recs) != 1 {
		t.Fatalf("записей %d, ждали 1: пропажу записи результата стереть не должна", len(recs))
	}
	if str(recs[0], "event") != agentExecEventIntent || str(recs[0], "exec_id") != execID {
		t.Errorf("уцелела не та запись: %+v", recs[0])
	}
}

func hasPython() bool {
	for _, name := range []string{"python3", "python"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}

// sleeperServer поднимает сервер с одним настоящим плагином, который спит
// secs секунд. Сон выбран как измеримый признак исполнения: он не зависит
// ни от формы конверта, ни от прав на диск, в отличие от проверки «появился
// ли файл-метка» — та проверка уже обманула один раз.
func sleeperServer(t *testing.T, slept float64) (*Server, string) {
	t.Helper()
	if !hasPython() {
		t.Skip("интерпретатор python не найден в PATH")
	}
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	work := filepath.Join(dir, "work")
	runs := filepath.Join(dir, "runs")
	for _, d := range []string{plugins, work, runs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFakePlugin(t, plugins, "sleeper",
		map[string]interface{}{"text": map[string]interface{}{"type": "string", "from": "input.text"}},
		map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}})
	py := "import json,sys,time\njson.load(sys.stdin)\ntime.sleep(" +
		strings.TrimRight(strings.TrimRight(fmtFloat(slept), "0"), ".") + ")\n" +
		"json.dump({'status':'ok','output':{'done':True}},sys.stdout)\n"
	if err := os.WriteFile(filepath.Join(plugins, "sleeper", "main.py"), []byte(py), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Options{
		PluginsDirs: []string{plugins},
		WorkDir:     work,
		RunsDir:     runs,
		// Фикстурный плагин доверен явно (по хэшу содержимого): после
		// инверсии (H1) он иначе внешний код, и на хостах без изолятора не
		// запустился бы вовсе — а тесту нужен реальный запуск.
		Trust: plugin.TrustPolicy{
			AgentCanExec:   true,
			AllowUntrusted: true,
			Trusted:        trustDirsIn(t, plugins),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, runs
}

func fmtFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// ГЛАВНЫЙ ТЕСТ ФАЙЛА. До перестановки записей он утверждал обратное: аудит
// писался после запуска, поэтому «не записалось» означало «результат не
// отдан», а код уже отработал.
//
// Теперь намерение пишется ДО запуска, и отказ по аудиту означает настоящий
// запрет. Проверяется измеримо: плагин спит 1.2 с, поэтому если бы он
// стартовал, вызов занял бы больше секунды. Возврат за десятки миллисекунд
// означает, что запуска не было.
func TestAgentExecAuditFailurePreventsExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("запускает настоящий плагин")
	}
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	work := filepath.Join(dir, "work")
	for _, d := range []string{plugins, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !hasPython() {
		t.Skip("интерпретатор python не найден в PATH")
	}
	writeFakePlugin(t, plugins, "sleeper",
		map[string]interface{}{"text": map[string]interface{}{"type": "string", "from": "input.text"}},
		map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}})
	py := "import json,sys,time\njson.load(sys.stdin)\ntime.sleep(1.2)\n" +
		"json.dump({'status':'ok','output':{'done':True}},sys.stdout)\n"
	if err := os.WriteFile(filepath.Join(plugins, "sleeper", "main.py"), []byte(py), 0o644); err != nil {
		t.Fatal(err)
	}

	// Каталог ранов нормальный, но на месте журнала — каталог: запись
	// невозможна, при этом NewServer проходит, иначе помеха поймалась бы на
	// конструкторе и до exec_plugin дело не дошло бы.
	runs := filepath.Join(dir, "runs")
	if err := os.MkdirAll(filepath.Join(runs, agentExecAuditFile), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Options{
		PluginsDirs: []string{plugins},
		WorkDir:     work,
		RunsDir:     runs,
		Trust: plugin.TrustPolicy{
			AgentCanExec:   true,
			AllowUntrusted: true,
			Trusted:        trustDirsIn(t, plugins),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	out, _, rpcErr := srv.toolExecPlugin(nil, map[string]interface{}{
		"plugin": "sleeper",
		"input":  map[string]interface{}{"text": "x"},
	})
	took := time.Since(t0)

	if rpcErr == nil {
		t.Fatalf("при недоступном аудите ожидался отказ; получено out=%q", out)
	}
	if code := rpcCodeOf(rpcErr); code != "E_AGENT_EXEC_AUDIT" {
		t.Errorf("код = %q, ждали E_AGENT_EXEC_AUDIT (msg=%q)", code, rpcErr.Message)
	}
	if out != "" {
		t.Errorf("агент не должен получать результат, получено: %q", out)
	}
	// Собственно проверка запрета. Плагин спал бы 1.2 с.
	if took >= time.Second {
		t.Fatalf("вызов занял %s — плагин похоже всё-таки запускался, "+
			"хотя аудит был недоступен. Запрет не работает.", took)
	}
}

// Успешный запуск: намерение и результат в журнале, порядок верный, а
// duration результата включает время сна плагина.
func TestAgentExecAuditRecordsBothAroundRealExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("запускает настоящий плагин")
	}
	srv, runs := sleeperServer(t, 1.2)

	if _, _, rpcErr := srv.toolExecPlugin(nil, map[string]interface{}{
		"plugin": "sleeper",
		"input":  map[string]interface{}{"text": "x"},
	}); rpcErr != nil {
		t.Fatalf("exec_plugin должен был отработать: %+v", rpcErr)
	}

	recs := readAudit(t, runs)
	if len(recs) != 2 {
		t.Fatalf("записей %d, ждали 2: %+v", len(recs), recs)
	}
	if str(recs[0], "event") != agentExecEventIntent {
		t.Errorf("первая запись = %q, ждали намерение — оно обязано быть до запуска",
			str(recs[0], "event"))
	}
	if str(recs[1], "event") != agentExecEventResult {
		t.Errorf("вторая запись = %q, ждали результат", str(recs[1], "event"))
	}
	d, err := time.ParseDuration(str(recs[1], "duration"))
	if err != nil {
		t.Fatalf("duration %q не разбирается: %v", str(recs[1], "duration"), err)
	}
	if d < time.Second {
		t.Errorf("duration результата = %s, а плагин спал 1.2 с", d)
	}
}
