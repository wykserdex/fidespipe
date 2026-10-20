package plugin

// v0.23: надёжность запуска плагина (лимит вывода, process-group kill).

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
	"gopkg.in/yaml.v3"
)

func requirePythonT(t *testing.T) {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if _, err := exec.LookPath(name); err == nil {
			return
		}
	}
	t.Skip("python не найден — пропускаю")
}

func fixtureManifest(t *testing.T, name string) *pipeline.Manifest {
	t.Helper()
	dir, _ := filepath.Abs(filepath.Join("..", "core", "testdata", "plugins", name))
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m pipeline.Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Dir = dir
	return &m
}

func TestCappedWriterKeepsPrefixAtLimit(t *testing.T) {
	var buf bytes.Buffer
	w := &cappedWriter{buf: &buf, limit: 5}
	if n, err := w.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("Write=%d err=%v", n, err)
	}
	if !w.overflow || buf.String() != "abcde" {
		t.Fatalf("capped writer=%q overflow=%v", buf.String(), w.overflow)
	}
}

// Гигантский stdout (17MB) — не «всю память», а честная ошибка протокола.
// TestExecStdoutCap
func TestExecStdoutCap(t *testing.T) {
	requirePythonT(t)
	m := fixtureManifest(t, "chatter")
	res := ExecWithEnvCtx(trustCtx(t, m), m, []byte("{}"), 30*time.Second, nil)
	if !res.Platform || res.ErrCode != "protocol_violation" {
		t.Fatalf("ожидался protocol_violation, got platform=%v err=%q msg=%q", res.Platform, res.ErrCode, res.ErrMsg)
	}
	if len(res.ErrMsg) == 0 || !strings.Contains(res.ErrMsg, "лимит") {
		t.Fatalf("сообщение не про лимит: %q", res.ErrMsg)
	}
}

// Таймаут убивает процесс-группу: дочерний sleep плагина не остаётся сиротой.
func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	requirePythonT(t)
	m := fixtureManifest(t, "spawner")
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	res := ExecWithEnvCtx(trustCtx(t, m), m, []byte("{}"), time.Second, []string{"SPID_FILE=" + pidFile})
	if res.ErrCode != "timeout" {
		t.Fatalf("ожидался timeout, got %q (%q)", res.ErrCode, res.ErrMsg)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("pid дочернего не записан: %v", err)
	}
	var pid int
	if _, err := parsePID(string(raw), &pid); err != nil {
		t.Fatal(err)
	}
	// до 5 секунд ждём смерти дочернего (group kill должен быть мгновенным)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return // умер — отлично
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("дочерний процесс %d жив после таймаута — process-group kill не сработал", pid)
}

func parsePID(s string, out *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			continue
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return n, nil
}

func TestBuiltinNamespaceIsClosed(t *testing.T) {
	eng := NewEngine()
	for _, ref := range []string{"core/human_gate", `core\human_gate`, "core/text_stats", `core\text_stats`} {
		manifest, err := eng.LoadManifest(ref)
		if err != nil {
			t.Fatalf("builtin %q: err=%v", ref, err)
		}
		// Раньше LoadManifest отдавал один и тот же манифест human_gate на любой
		// ref из core/, потому что builtin был ровно один. С появлением
		// core/text_stats манифест обязан совпадать с запрошенным модулем,
		// иначе шаг получил бы чужой контракт.
		want := strings.ReplaceAll(ref, `\`, "/")
		if manifest.ID != want {
			t.Fatalf("builtin %q: manifest.ID=%q, ожидалось %q", ref, manifest.ID, want)
		}
	}
	// Контракт data-модуля обязан быть непустым: пустой манифест означал бы, что
	// выход не проверяется по типам и bind не проверяется по портам.
	ts, err := eng.LoadManifest("core/text_stats")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.Input) == 0 || len(ts.Output) == 0 {
		t.Fatalf("контракт core/text_stats пуст: input=%v output=%v", ts.Input, ts.Output)
	}
	if _, ok := ts.Input["text"]; !ok {
		t.Errorf("в core/text_stats нет входа text: %v", ts.Input)
	}
	for _, port := range []string{"lines", "words", "unique_words", "longest_word"} {
		if _, ok := ts.Output[port]; !ok {
			t.Errorf("в core/text_stats нет выхода %q: %v", port, ts.Output)
		}
	}
	// У встроенного модуля нет ни сети, ни секретов, ни файлов: он доверенный
	// код в процессе ядра, и declare-now-проверки на нём не должны срабатывать.
	if len(ts.Permissions.Network) != 0 || len(ts.Permissions.Secrets) != 0 {
		t.Errorf("встроенный модуль не должен ничего заявлять: %+v", ts.Permissions)
	}
	for _, ref := range []string{"core/does_not_exist", "core/human_gate/extra", "core", "core/text_statz"} {
		if _, err := eng.LoadManifest(ref); err == nil {
			t.Fatalf("reserved builtin %q was accepted", ref)
		}
	}
}

func TestPluginOnlyReceivesDeclaredSecrets(t *testing.T) {
	requirePythonT(t)
	t.Setenv("FIDESPIPE_TEST_DECLARED_SECRET", "declared")
	t.Setenv("FIDESPIPE_TEST_UNRELATED_SECRET", "leaked")
	dir := t.TempDir()
	script := "import os,sys,json\njson.load(sys.stdin)\njson.dump({'status':'ok','output':{'declared':os.environ.get('FIDESPIPE_TEST_DECLARED_SECRET',''),'unrelated':os.environ.get('FIDESPIPE_TEST_UNRELATED_SECRET','')}},sys.stdout)\n"
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	m := &pipeline.Manifest{
		ID:      "env-test",
		Runtime: pipeline.Runtime{Type: "python", Entry: "main.py"},
		Dir:     dir,
		Output: map[string]pipeline.Port{
			"declared":  {Type: "string"},
			"unrelated": {Type: "string"},
		},
		Permissions: pipeline.Permissions{Secrets: []string{"FIDESPIPE_TEST_DECLARED_SECRET"}},
	}
	res := ExecWithEnvCtx(trustCtx(t, m), m, []byte("{}"), 10*time.Second, nil)
	if !res.OK() {
		t.Fatalf("plugin failed: %+v", res)
	}
	if res.Output["declared"] != "declared" {
		t.Fatalf("declared secret=%v", res.Output["declared"])
	}
	if res.Output["unrelated"] != "" {
		t.Fatalf("unrelated environment leaked: %v", res.Output["unrelated"])
	}
}

// ── retry-политика результата (PROTOCOL §3/§6) ──────────────────────────
//
// Платформенная ошибка (exit>=2, таймаут, невалидный stdout) останавливает
// ран всегда: retryable от плагина на exit>=2 политикой не переопределяется.
// Ретраится только таймаут и доменная ошибка с retryable: true.

func TestShouldRetryResult(t *testing.T) {
	cases := []struct {
		name string
		res  ExecResult
		want bool
	}{
		{"timeout ретраится", ExecResult{Platform: true, ErrCode: "timeout", ExitCode: 2, Retryable: true}, true},
		{"доменная retryable", ExecResult{Platform: false, ErrCode: "rate_limit", ExitCode: 1, Retryable: true}, true},
		{"доменная без retryable", ExecResult{Platform: false, ErrCode: "bad_value", ExitCode: 1}, false},
		{"exit>=2 с retryable:true — stop", ExecResult{Platform: true, ErrCode: "platform:bad_input", ExitCode: 2, Retryable: true}, false},
		{"краш без кода — stop", ExecResult{Platform: true, ErrCode: "crash", ExitCode: 2, Retryable: true}, false},
		{"нарушение протокола — stop", ExecResult{Platform: true, ErrCode: "protocol_violation", ExitCode: 2, Retryable: true}, false},
		{"отмена — stop", ExecResult{Platform: true, Cancelled: true, ErrCode: "cancelled", ExitCode: 2, Retryable: true}, false},
	}
	for _, c := range cases {
		if got := c.res.ShouldRetry(); got != c.want {
			t.Errorf("%s: ShouldRetry()=%v, want %v (res=%+v)", c.name, got, c.want, c.res)
		}
	}
}

// PlatformErrCode ставит префикс `platform:` ровно один раз.
func TestPlatformErrCode(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{"bad_input", "platform:bad_input"},
		{"platform:bad_input", "platform:bad_input"},
		{"timeout", "platform:timeout"},
		{"", ""},
	}
	for _, c := range cases {
		if got := PlatformErrCode(c.code); got != c.want {
			t.Errorf("PlatformErrCode(%q)=%q, want %q", c.code, got, c.want)
		}
	}
}

// manifestExitingWith — временный плагин: печатает конверт с error.code и
// выходит с exitCode. Нужен для ветки exit>=2 в execPluginEnv.
func manifestExitingWith(t *testing.T, code string, exitCode int, retryable bool) *pipeline.Manifest {
	t.Helper()
	envelope, err := json.Marshal(map[string]interface{}{
		"status": "error",
		"error":  map[string]interface{}{"code": code, "message": "boom", "retryable": retryable},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "import json,sys\njson.load(sys.stdin)\n" +
		"sys.stdout.write(" + strconv.Quote(string(envelope)) + ")\n" +
		"sys.exit(" + strconv.Itoa(exitCode) + ")\n"
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0644); err != nil {
		t.Fatal(err)
	}
	return &pipeline.Manifest{
		ID:      "exiting",
		Runtime: pipeline.Runtime{Type: "python", Entry: "main.py"},
		Dir:     dir,
		Output:  map[string]pipeline.Port{"value": {Type: "string"}},
	}
}

// exit>=2 + retryable:true → платформенная ошибка без повторов, с одним
// префиксом `platform:` (PROTOCOL §3).
func TestExecExit2WithRetryableIsPlatformAndNotRetried(t *testing.T) {
	requirePythonT(t)
	for _, c := range []struct {
		pluginCode string
		wantCode   string
	}{
		{"bad_input", "platform:bad_input"},
		{"platform:bad_input", "platform:bad_input"},
	} {
		m := manifestExitingWith(t, c.pluginCode, 2, true)
		res := ExecWithEnvCtx(trustCtx(t, m), m, []byte("{}"), 15*time.Second, nil)
		if !res.Platform {
			t.Fatalf("code=%q: exit>=2 обязан быть платформенной ошибкой: %+v", c.pluginCode, res)
		}
		if res.ErrCode != c.wantCode {
			t.Fatalf("code=%q: ErrCode=%q, want %q (без двойного префикса)", c.pluginCode, res.ErrCode, c.wantCode)
		}
		if !strings.HasPrefix(res.ErrCode, "platform:") {
			t.Fatalf("code=%q: платформенный код без префикса: %q", c.pluginCode, res.ErrCode)
		}
		if res.Retryable != true {
			t.Fatalf("code=%q: retryable из конверта должен попасть в журнал для триажа: %+v", c.pluginCode, res)
		}
		if res.ShouldRetry() {
			t.Fatalf("code=%q: exit>=2 с retryable=true обязан стопить ран, а не ретраиться", c.pluginCode)
		}
	}
}

// Доменная ошибка (exit 1) с retryable:true по-прежнему ретраится — граница
// проходит по exit-коду, а не по флагу.
func TestExecExit1RetryableStillRetried(t *testing.T) {
	requirePythonT(t)
	m := manifestExitingWith(t, "rate_limit", 1, true)
	res := ExecWithEnvCtx(trustCtx(t, m), m, []byte("{}"), 15*time.Second, nil)
	if res.OK() || res.Platform {
		t.Fatalf("exit 1 — доменная ошибка, не платформенная: %+v", res)
	}
	if res.ErrCode != "rate_limit" || !res.Retryable {
		t.Fatalf("конверт exit 1 не разобран: %+v", res)
	}
	if !res.ShouldRetry() {
		t.Fatal("доменная ошибка с retryable:true обязана ретраиться")
	}
}
