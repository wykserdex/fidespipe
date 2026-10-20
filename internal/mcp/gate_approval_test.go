package mcp

// Требование одобрения человека в run_pipeline (внешний аудит, H2).
//
// Проверяется ровно то, чего раньше не было: пайплайн БЕЗ human_gate, в котором
// есть опасный шаг, отклоняется, а не исполняется молча. Опасность читается из
// объявленных capabilities плагина, а не из наличия гейта: гейт после
// опасного шага не защищает.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/gate"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

// writeGatePlugin — плагин с заданными permissions. Права задаются явно,
// потому что весь смысл проверки в том, что она читает объявление.
func writeGatePlugin(t *testing.T, pluginsDir, id string, perms map[string]interface{}) string {
	t.Helper()
	dir := filepath.Join(pluginsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"id": id, "version": "0.1.0", "platform_api": "0.1",
		"runtime":     map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":       map[string]interface{}{},
		"output":      map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}},
		"permissions": perms,
		"description": "фикстура теста требования гейта",
		"author":      "fidespipe tests",
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	script := "import sys,json\njson.load(sys.stdin)\njson.dump({'status':'ok','output':{'done':True}},sys.stdout)\n"
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// dangerPerms / safePerms — объявления, которые различают опасный и безопасный
// шаг. Ни сети (MCP отказывает любой сетевой декларации раньше), ни файлов:
// опасность здесь держится на объявленных секретах, а «безопасность» — на
// полном отсутствии объявлений.
func dangerPerms() map[string]interface{} {
	return map[string]interface{}{
		"network":    []interface{}{},
		"filesystem": "readwrite",
		"secrets":    []string{"GATE_TEST_TOKEN"},
	}
}

func safePerms() map[string]interface{} {
	return map[string]interface{}{
		"network":    []interface{}{},
		"filesystem": "none",
		"secrets":    []interface{}{},
	}
}

type gateServerOpts struct {
	human         HumanChannel
	allowUngated  bool
	dangerous     bool
	extraSafeStep bool
}

// gateServer — сервер MCP с одним плагином нужного класса прав. Вторым
// значением возвращается каталог плагина: он же ссылка в YAML.
func gateServer(t *testing.T, opts gateServerOpts) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	work := filepath.Join(root, "work")
	for _, d := range []string{plugins, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	perms := safePerms()
	if opts.dangerous {
		perms = dangerPerms()
	}
	pluginDir := writeGatePlugin(t, plugins, "stepper", perms)
	// Плагин, написанный тестом, ни в чьём allow-list не значится, поэтому
	// после введения доверия он недоверенный: без изолятора (а на Windows её
	// нет) запуск обязан быть отклонён. Тест проверяет ГЕЙТ, а не доверие,
	// поэтому доверие выдаётся явно — ровно так же, как это сделал бы
	// оператор через fidespipe-trust.yaml. Ослаблять политику ради теста нельзя:
	// тогда тест гейта начал бы проходить на коде, который в жизни не
	// запустится.
	allow, err := plugin.AllowListFromDirs(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	if opts.extraSafeStep {
		writeGatePlugin(t, plugins, "counter", safePerms())
	}
	srv, err := NewServer(Options{
		PluginsDirs:      []string{plugins},
		WorkDir:          work,
		Human:            opts.human,
		AllowUngatedRuns: opts.allowUngated,
		Trust:            plugin.TrustPolicy{Trusted: allow},
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, pluginDir
}

// fakeHuman — канал к человеку для тестов. Умеет подтвердить гейт сам, чтобы
// проверка «гейт есть и канал есть» доводила ран до конца, а не висела.
type fakeHuman struct {
	mu        sync.Mutex
	ui        *gate.ChannelUI
	waitingOn []string
	accept    bool
	once      sync.Once
}

func (f *fakeHuman) AttachRun(_ string, ui *gate.ChannelUI, _ context.CancelFunc) {
	if ui == nil {
		return
	}
	f.mu.Lock()
	f.ui = ui
	f.mu.Unlock()
	if !f.accept {
		return
	}
	f.once.Do(func() {
		go func() {
			// Решение приходит после gate_wait: ChannelUI буферизует один
			// сигнал, но отправка до ожидания всё равно не теряется.
			time.Sleep(50 * time.Millisecond)
			ui.SendDecision(gate.Decision{Action: "accept"})
		}()
	})
}

func (f *fakeHuman) DetachRun(string) {}

func (f *fakeHuman) GateWaiting(runID string) {
	f.mu.Lock()
	f.waitingOn = append(f.waitingOn, runID)
	f.mu.Unlock()
}

func (f *fakeHuman) PublicURL(runID string) string { return "http://127.0.0.1:0/?run=" + runID }

func (f *fakeHuman) sawGate() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waitingOn) > 0
}

func pipelineYAML(name, pluginDir string, withGate bool) string {
	yaml := "format_version: \"0.2\"\npipeline:\n  name: " + name + "\n  input: {}\n  steps:\n"
	if withGate {
		yaml += "    - id: review\n      plugin: core/human_gate\n      form: []\n" +
			"      actions: [accept, reject]\n      on_reject: stop\n"
	}
	yaml += "    - id: step\n      plugin: " + pluginDir + "\n"
	return yaml
}

func startRun(t *testing.T, srv *Server, yaml string) (string, string, *RPCError) {
	t.Helper()
	out, _, rpcErr := srv.callTool(nil, "run_pipeline", map[string]interface{}{"yaml": yaml, "wait_seconds": 2.0})
	if rpcErr != nil {
		return "", "", rpcErr
	}
	var parsed struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("ответ run_pipeline не разобран: %v (%s)", err, out)
	}
	return parsed.RunID, parsed.Status, nil
}

// Пайплайн без гейта, где есть опасный шаг → отказ ДО старта. Это и есть
// исходная дыра: раньше такой ран исполнялся целиком, и «у агента нет кнопки
// approve» было верно только потому, что гейт можно было не включить.
func TestRunPipelineRefusesUngatedDangerousStep(t *testing.T) {
	srv, pluginDir := gateServer(t, gateServerOpts{dangerous: true})
	runID, _, rpcErr := startRun(t, srv, pipelineYAML("ungated_danger", pluginDir, false))
	if rpcErr == nil {
		t.Fatalf("пайплайн с опасным шагом без гейта запустился (run_id=%q)", runID)
	}
	if code := rpcCodeOf(rpcErr); code != "E_GATE_REQUIRED" {
		t.Fatalf("Data[code] = %q, ждали E_GATE_REQUIRED (message: %s)", code, rpcErr.Message)
	}
	if !strings.Contains(rpcErr.Message, "core/human_gate") {
		t.Errorf("отказ должен называть, что делать (добавить core/human_gate): %s", rpcErr.Message)
	}
	if !strings.Contains(rpcErr.Message, "чтение секретов") {
		t.Errorf("отказ должен называть конкретные права опасного шага: %s", rpcErr.Message)
	}
	// Никакого рана не создано: отказ до побочных эффектов.
	if entries, err := os.ReadDir(srv.runsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				t.Errorf("отказ не должен оставлять каталог рана: %s", e.Name())
			}
		}
	}
}

// Пайплайн без гейта, где все шаги безопасны → разрешён. Иначе требование
// было бы слишком широким: агент не смог бы посчитать слова или разобрать
// файл, и правило превратилось бы в запрет на полезную работу.
func TestRunPipelineAllowsUngatedSafeSteps(t *testing.T) {
	srv, pluginDir := gateServer(t, gateServerOpts{})
	runID, _, rpcErr := startRun(t, srv, pipelineYAML("ungated_safe", pluginDir, false))
	if rpcErr != nil {
		t.Fatalf("безопасный пайплайн без гейта должен быть разрешён: %v (%s)", rpcErr.Message, rpcErr.Message)
	}
	if runID == "" {
		t.Fatal("безопасный пайплайн должен стартовать и вернуть run_id")
	}
	waitRunTerminal(t, srv, runID)
}

// Гейт есть и канал к человеку есть → обычный путь: ран доходит до гейта,
// человек (в тесте — fakeHuman) решает, цепочка идёт дальше и завершается.
func TestRunPipelineWithGateAndHumanChannelRunsNormally(t *testing.T) {
	human := &fakeHuman{accept: true}
	srv, pluginDir := gateServer(t, gateServerOpts{human: human, dangerous: true})
	runID, status, rpcErr := startRun(t, srv, pipelineYAML("gated_danger", pluginDir, true))
	if rpcErr != nil {
		t.Fatalf("пайплайн с гейтом и каналом к человеку должен идти обычным путём: %v", rpcErr)
	}
	if runID == "" {
		t.Fatal("ожидался run_id")
	}
	final := waitRunTerminal(t, srv, runID)
	if final != "done" {
		t.Fatalf("ран с одобренным гейтом завершился как %q (промежуточный статус %q)", final, status)
	}
	if !human.sawGate() {
		t.Error("канал к человеку не увидел гейта — одобрение не было запрошено")
	}
}

// Гейт есть, а канала к человеку нет → E_NO_HUMAN_CHANNEL. Существующее
// поведение не сломано: сервер без консоли не может ждать вечно.
func TestRunPipelineGateWithoutHumanChannelRefused(t *testing.T) {
	srv, pluginDir := gateServer(t, gateServerOpts{dangerous: true})
	_, _, rpcErr := startRun(t, srv, pipelineYAML("gated_no_channel", pluginDir, true))
	if rpcErr == nil {
		t.Fatal("пайплайн с гейтом без канала к человеку должен отклоняться")
	}
	if code := rpcCodeOf(rpcErr); code != "E_NO_HUMAN_CHANNEL" {
		t.Fatalf("Data[code] = %q, ждали E_NO_HUMAN_CHANNEL (message: %s)", code, rpcErr.Message)
	}
}

// core/text_stats — встроенный модуль, но НЕ гейт. Раньше он считался гейтом
// (IsBuiltin истинен и для него), и пайплайн без канала к человеку отклонялся
// с E_NO_HUMAN_CHANNEL, хотя одобрять в нём нечего.
func TestTextStatsStepIsNotTreatedAsHumanGate(t *testing.T) {
	srv, pluginDir := gateServer(t, gateServerOpts{extraSafeStep: true})
	yaml := "format_version: \"0.2\"\npipeline:\n  name: stats_only\n  input: {}\n  steps:\n" +
		"    - id: stats\n      plugin: core/text_stats\n" +
		"    - id: step\n      plugin: " + pluginDir + "\n"
	runID, _, rpcErr := startRun(t, srv, yaml)
	if rpcErr != nil {
		if rpcCodeOf(rpcErr) == "E_NO_HUMAN_CHANNEL" {
			t.Fatalf("core/text_stats не должен считаться гейтом: %s", rpcErr.Message)
		}
		t.Fatalf("неожиданный отказ: %v (%s)", rpcErr, rpcErr.Message)
	}
	if runID == "" {
		t.Fatal("ожидался run_id")
	}
	waitRunTerminal(t, srv, runID)
}

// Операторский обход работает, но оставляет след: строка в append-only журнале с
// именем опасного шага и его правами. Обход без следа — это тот самый тихий
// режим, ради которого проверка и добавлена.
func TestOperatorBypassRunsUngatedAndLeavesAudit(t *testing.T) {
	srv, pluginDir := gateServer(t, gateServerOpts{allowUngated: true, dangerous: true})
	runID, _, rpcErr := startRun(t, srv, pipelineYAML("bypassed", pluginDir, false))
	if rpcErr != nil {
		t.Fatalf("с --allow-unapproved-runs ран должен идти: %v (%s)", rpcErr, rpcErr.Message)
	}
	if runID == "" {
		t.Fatal("ожидался run_id")
	}
	waitRunTerminal(t, srv, runID)

	recs := readBypassAudit(t, srv.runsDir)
	if len(recs) != 1 {
		t.Fatalf("записей обхода %d, ждали 1", len(recs))
	}
	rec := recs[0]
	if str(rec, "event") != gateBypassEvent {
		t.Errorf("event = %q, ждали %q", str(rec, "event"), gateBypassEvent)
	}
	if str(rec, "step") != "step" || str(rec, "pipeline") != "bypassed" {
		t.Errorf("запись не называет, что именно пошло без одобрения: %+v", rec)
	}
	if str(rec, "capabilities") == "" {
		t.Error("запись должна называть права опасного шага, а не просто факт обхода")
	}
	if str(rec, "flag") != "--allow-unapproved-runs" {
		t.Errorf("запись должна называть флаг обхода: %+v", rec)
	}
	if str(rec, "ts") == "" {
		t.Error("у записи обхода должна быть метка времени")
	}
}

// Обход без следа недопустим: если журнал недоступен, ран не идёт. Иначе
// «операторский обход» был бы обычным тихим путём.
func TestOperatorBypassRefusesWhenAuditCannotBeWritten(t *testing.T) {
	srv, pluginDir := gateServer(t, gateServerOpts{allowUngated: true, dangerous: true})
	// Подменяем каталог ранов файлом: журнал открыть нельзя ни на Unix, ни на
	// Windows (тест проходит одинаково на обеих платформах).
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.runsDir = blocker
	_, _, rpcErr := startRun(t, srv, pipelineYAML("bypass_no_audit", pluginDir, false))
	if rpcErr == nil {
		t.Fatal("обход без записи в аудит обязан отказать, а не идти молча")
	}
	if code := rpcCodeOf(rpcErr); code != "E_GATE_REQUIRED" {
		t.Errorf("Data[code] = %q, ждали E_GATE_REQUIRED (требование остаётся в силе)", code)
	}
	if !strings.Contains(rpcErr.Message, gateBypassAuditFile) {
		t.Errorf("отказ должен называть журнал, который не записался: %s", rpcErr.Message)
	}
}

// Гейт после опасного шага ничего не одобряет: человек увидит результат, а не
// намерение. Отказ обязан сказать об этом прямо, иначе агент поставит гейт в
// конец и будет считать, что требование выполнено.
func TestGateAfterDangerousStepIsNamedAsTooLate(t *testing.T) {
	srv, pluginDir := gateServer(t, gateServerOpts{human: &fakeHuman{accept: true}, dangerous: true})
	yaml := "format_version: \"0.2\"\npipeline:\n  name: late_gate\n  input: {}\n  steps:\n" +
		"    - id: step\n      plugin: " + pluginDir + "\n" +
		"    - id: review\n      plugin: core/human_gate\n      form: []\n      actions: [accept, reject]\n"
	_, _, rpcErr := startRun(t, srv, yaml)
	if rpcErr == nil {
		t.Fatal("гейт после опасного шага не должен считаться одобрением")
	}
	if code := rpcCodeOf(rpcErr); code != "E_GATE_REQUIRED" {
		t.Fatalf("Data[code] = %q, ждали E_GATE_REQUIRED", code)
	}
	if !strings.Contains(rpcErr.Message, "ПОСЛЕ") || !strings.Contains(rpcErr.Message, "review") {
		t.Errorf("отказ должен сказать, что гейт review стоит слишком поздно: %s", rpcErr.Message)
	}
}

func readBypassAudit(t *testing.T, dir string) []map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, gateBypassAuditFile))
	if err != nil {
		t.Fatalf("журнал обходов не прочитан: %v", err)
	}
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("строка журнала не разбирается: %v\n%s", err, line)
		}
		out = append(out, rec)
	}
	return out
}

// waitRunTerminal — дождаться терминального статуна. Ран живёт в горутине, и
// тест, уйдя раньше, оставил бы за собой процесс плагина и незакрытый
// каталог (на Windows это ещё и упало бы на cleanup).
func waitRunTerminal(t *testing.T, srv *Server, runID string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		out, _, rpcErr := srv.callTool(nil, "get_run", map[string]interface{}{"run_id": runID})
		if rpcErr != nil {
			t.Fatalf("get_run: %v", rpcErr)
		}
		var parsed struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("ответ get_run не разобран: %v", err)
		}
		last = parsed.Status
		switch last {
		case "done", "failed", "cancelled":
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("ран %s не пришёл в терминальный статус за 30с (последний %q)", runID, last)
	return last
}

// Значения прав, на которых строится проверка, должны совпадать с тем, что
// считает ядро: иначе тесты MCP проверяли бы не тот код.
func TestGatePolicyAndServerShareCapabilities(t *testing.T) {
	m := &pipeline.Manifest{ID: "p", Permissions: pipeline.Permissions{
		Filesystem: "readwrite", Secrets: []string{"TOKEN"}}}
	caps := pipeline.StepCapabilities(&pipeline.Step{ID: "s", Plugin: "p"}, m)
	if !caps.Dangerous() {
		t.Fatal("readwrite+secrets обязаны считаться опасными")
	}
	if caps.Why() == "" {
		t.Error("Why() обязан быть непустым")
	}
}

// Гейт, который может не выполниться или чей отказ не останавливает ран, не
// считается одобрением: MCP отказывает до старта, как и без гейта вовсе.
func TestRunPipelineRefusesIneffectiveGate(t *testing.T) {
	cases := map[string]string{
		"when":      "      when: {path: input.t, op: eq, value: never}\n",
		"on_reject": "      on_reject: continue\n",
		"on_error":  "      on_error: skip\n",
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			srv, pluginDir := gateServer(t, gateServerOpts{human: &fakeHuman{accept: true}, dangerous: true})
			yaml := "format_version: \"0.2\"\npipeline:\n  name: ineffective_" + name + "\n  input: {t: a}\n  steps:\n" +
				"    - id: review\n      plugin: core/human_gate\n      form: []\n      actions: [accept, reject]\n" + extra +
				"    - id: step\n      plugin: " + pluginDir + "\n"
			runID, _, rpcErr := startRun(t, srv, yaml)
			if rpcErr == nil {
				t.Fatalf("гейт с %s не гарантирует одобрение, а ран стартовал (run_id=%q)", name, runID)
			}
			if code := rpcCodeOf(rpcErr); code != "E_GATE_REQUIRED" {
				t.Fatalf("code = %q, ждали E_GATE_REQUIRED (%s)", code, rpcErr.Message)
			}
			if !strings.Contains(rpcErr.Message, "review") || !strings.Contains(rpcErr.Message, "не гарантирует") {
				t.Errorf("отказ должен называть гейт и причину: %s", rpcErr.Message)
			}
		})
	}
}
