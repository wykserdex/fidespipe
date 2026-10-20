package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/plugin"
)

func TestPruneRunsKeepsBoundedCompletedState(t *testing.T) {
	srv := &Server{runs: map[string]*runState{}}
	for i := 0; i < maxRetainedRuns; i++ {
		done := make(chan struct{})
		close(done)
		srv.runs[strconv.Itoa(i)] = &runState{done: done}
	}
	active := make(chan struct{})
	srv.runs["active"] = &runState{done: active}
	srv.pruneRunsLocked()
	if len(srv.runs) >= maxRetainedRuns+1 {
		t.Fatalf("completed runs were not pruned: %d", len(srv.runs))
	}
	if _, ok := srv.runs["active"]; !ok {
		t.Fatal("active run was pruned")
	}
}

func writeFakePlugin(t *testing.T, dir, id string, input, output map[string]interface{}) {
	t.Helper()
	d := filepath.Join(dir, id)
	if err := os.MkdirAll(d, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"id": id, "version": "0.1", "platform_api": "0.1",
		"runtime": map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":   input, "output": output,
		"permissions": map[string]interface{}{
			// Права не объявлены НИКАКИЕ — по двум причинам, и обе важны.
			//
			// Первая (сеть): фикстура проверяет механики MCP (запуск,
			// разрешение путей, границы wait, file_ref), а не сетевую политику.
			// У MCP собственная проверка жёстче рантаймной (checkPipelineSafety
			// отказывает любой сетевой декларации), поэтому плагин с сетью здесь
			// ломал бы тест не по своему предмету. Сетевые кейсы — через
			// writePolicyPlugin.
			//
			// Вторая (диск/секреты): run_pipeline требует human_gate перед
			// первым шагом с такими правами, и объявление их здесь ломало бы
			// тесты, предмет которых — запуск без гейта. Плагин читает stdin и
			// пишет stdout, то есть прав не заявляет; объявлять неиспользуемое
			// было бы враньём манифеста, на которое смотрят другие проверки.
			"network":    []map[string]interface{}{},
			"filesystem": "none", "secrets": []string{},
		},
	}
	raw, _ := json.Marshal(manifest)
	// plugin.yaml нужен в YAML, но JSON — валидный YAML тоже
	if err := os.WriteFile(filepath.Join(d, "plugin.yaml"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	py := "import sys,json\njson.load(sys.stdin)\njson.dump({'status':'ok','output':{'done':True}},sys.stdout)\n"
	if err := os.WriteFile(filepath.Join(d, "main.py"), []byte(py), 0644); err != nil {
		t.Fatal(err)
	}
}

func writePolicyPlugin(t *testing.T, pluginsDir, id string, network []map[string]interface{}, input map[string]interface{}) string {
	t.Helper()
	dir := filepath.Join(pluginsDir, id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"id": id, "version": "0.1", "platform_api": "0.1",
		"runtime": map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":   input,
		"output":  map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}},
		"permissions": map[string]interface{}{
			"network": network, "filesystem": "workspace", "secrets": []string{},
		},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("import json,sys\njson.load(sys.stdin)\njson.dump({'status':'ok','output':{'done':True}},sys.stdout)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	work := filepath.Join(dir, "work")
	os.MkdirAll(plugins, 0755)
	os.MkdirAll(work, 0755)
	writeFakePlugin(t, plugins, "echoer",
		map[string]interface{}{"text": map[string]interface{}{"type": "string", "from": "input.text"}},
		map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}})
	// Фикстурный плагин доверен явно (по хэшу содержимого): после инверсии
	// доверия (H1) он иначе внешний код, и на хостах без изолятора не запустился
	// бы вовсе — а тесты этого сервера проверяют прочее.
	srv, err := NewServer(Options{
		PluginsDirs: []string{plugins},
		WorkDir:     work,
		Trust:       plugin.TrustPolicy{Trusted: trustDirsIn(t, plugins)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestMCPInitializeNegotiatesAndExposesPermissions(t *testing.T) {
	srv := testServer(t)
	resp := srv.handle(&Request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2026-07-28"}`),
	})
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	result, _ := json.Marshal(resp.Result)
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(result, &init); err != nil {
		t.Fatal(err)
	}
	// Клиент просил 2026-07-28, которой сервер не реализует (ревизия убрала
	// хендшейк initialize). Раньше в ответ уходила САМАЯ СТАРАЯ поддерживаемая
	// ревизия, и клиент не мог отличить понижение от согласия. Теперь — самая
	// новая из поддерживаемых.
	if init.ProtocolVersion != "2025-06-18" {
		t.Fatalf("unexpected protocol version: %q", init.ProtocolVersion)
	}
	// А ревизия, которую сервер поддерживает, обязана возвращаться как есть:
	// клиент на 2024-11-05 не должен получить чужую версию.
	resp2024 := srv.handle(&Request{
		JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2024-11-05"}`),
	})
	raw2024, _ := json.Marshal(resp2024.Result)
	var init2024 struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw2024, &init2024); err != nil {
		t.Fatal(err)
	}
	if init2024.ProtocolVersion != "2024-11-05" {
		t.Fatalf("поддерживаемая версия не подтверждена эхом: %q", init2024.ProtocolVersion)
	}

	listed, _, rpcErr := srv.callTool(nil, "list_plugins", map[string]interface{}{})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	var listOut struct {
		Plugins []struct {
			Permissions map[string]interface{} `json:"permissions"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(listed), &listOut); err != nil {
		t.Fatal(err)
	}
	if len(listOut.Plugins) != 1 {
		t.Fatalf("unexpected plugin list: %s", listed)
	}
	for _, key := range []string{"network", "filesystem", "secrets"} {
		if _, ok := listOut.Plugins[0].Permissions[key]; !ok {
			t.Fatalf("permission %q missing: %s", key, listed)
		}
	}
}

func TestMCPTransportPreservesStringID(t *testing.T) {
	tr := NewTransport(bytes.NewBufferString(`{"jsonrpc":"2.0","id":"abc","method":"ping"}`+"\n"), &bytes.Buffer{})
	req, err := tr.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(req.ID) != `"abc"` {
		t.Fatalf("unexpected request id: %s", req.ID)
	}
}

func TestMCPValidatePortSource(t *testing.T) {
	srv := testServer(t)
	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: bad\n  input:\n    email: \"a@b.c\"\n  steps:\n    - id: s\n      plugin: " + srv.pluginsDirs[0] + "/echoer\n      bind:\n        text: input.nope\n"
	res, isErr, rpcErr := srv.callTool(nil, "validate_pipeline", map[string]interface{}{"yaml": yamlStr})
	if rpcErr != nil {
		t.Fatalf("rpc: %v", rpcErr)
	}
	if isErr {
		t.Fatalf("isError: %s", res)
	}
	var out struct {
		OK     bool `json:"ok"`
		Issues []struct {
			Code string `json:"code"`
		} `json:"issues"`
	}
	_ = json.Unmarshal([]byte(res), &out)
	if out.OK {
		t.Fatalf("want ok:false, got %s", res)
	}
	found := false
	for _, issue := range out.Issues {
		if issue.Code == "E_PORT_SOURCE" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("want E_PORT_SOURCE, got %s", res)
	}
}

func TestMCPToolsListAndRun(t *testing.T) {
	srv := testServer(t)
	res, _, rpcErr := srv.callTool(nil, "list_plugins", map[string]interface{}{})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !strings.Contains(res, "echoer") {
		t.Fatalf("list_plugins без echoer: %s", res)
	}
	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: simple\n  input:\n    text: \"hello\"\n  steps:\n    - id: s\n      plugin: " + srv.pluginsDirs[0] + "/echoer\n      bind:\n        text: input.text\n"
	res, _, rpcErr = srv.callTool(nil, "run_pipeline", map[string]interface{}{"yaml": yamlStr, "wait_seconds": 15.0})
	if rpcErr != nil {
		t.Fatalf("run: %v", rpcErr)
	}
	var runOut struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal([]byte(res), &runOut)
	if runOut.RunID == "" {
		t.Fatalf("нет run_id: %s", res)
	}
	// дождаться done через get_run
	deadline := time.Now().Add(20 * time.Second)
	for {
		res2, _, rpcErr := srv.callTool(nil, "get_run", map[string]interface{}{"run_id": runOut.RunID})
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		var g struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal([]byte(res2), &g)
		if g.Status == "done" {
			return
		}
		if g.Status == "failed" {
			t.Fatalf("ран failed: %s", res2)
		}
		if time.Now().After(deadline) {
			t.Fatalf("ран не done за 20c: %s", res2)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestMCPGetRunRejectsUnsafeRunID(t *testing.T) {
	srv := testServer(t)
	outside := filepath.Join(srv.runsDir, "..", "outside")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "journal.jsonl"), []byte("{\"type\":\"run_end\"}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		runID string
	}{
		{"forward-parent", "../outside"},
		{"backslash-parent", `..\outside`},
		{"forward-child", "safe/child"},
		{"backslash-child", `safe\child`},
		{"forward-absolute", "/outside"},
		{"backslash-absolute", `\outside`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _, rpcErr := srv.callTool(nil, "get_run", map[string]interface{}{"run_id": tc.runID})
			if rpcErr == nil {
				t.Fatalf("get_run accepted unsafe run_id %q: %s", tc.runID, result)
			}
			if !strings.Contains(rpcErr.Message, "небезопасный run_id") {
				t.Fatalf("get_run error for %q = %q", tc.runID, rpcErr.Message)
			}
			if result != "" {
				t.Fatalf("get_run returned data for rejected run_id %q: %s", tc.runID, result)
			}
		})
	}
}

func TestMCPRejectsUnboundedWait(t *testing.T) {
	srv := testServer(t)
	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: bounded_wait\n  input:\n    text: hello\n  steps:\n    - id: s\n      plugin: " + srv.pluginsDirs[0] + "/echoer\n      bind:\n        text: input.text\n"
	_, _, rpcErr := srv.callTool(nil, "run_pipeline", map[string]interface{}{"yaml": yamlStr, "wait_seconds": float64(maxWaitSeconds) + 1})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "wait_seconds") {
		t.Fatalf("unbounded wait accepted: %v", rpcErr)
	}
}

func TestMCPResolvesRelativePluginFromWorkDir(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	plugins := filepath.Join(work, "plugins")
	if err := os.MkdirAll(plugins, 0755); err != nil {
		t.Fatal(err)
	}
	writeFakePlugin(t, plugins, "echoer",
		map[string]interface{}{"text": map[string]interface{}{"type": "string", "from": "input.text"}},
		map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}})
	srv, err := NewServer(Options{PluginsDirs: []string{"plugins"}, WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	otherCWD := filepath.Join(root, "other")
	if err := os.MkdirAll(otherCWD, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(otherCWD); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldCWD)

	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: relative_plugin\n  input:\n    text: \"hello\"\n  steps:\n    - id: s\n      plugin: plugins/echoer\n      bind:\n        text: input.text\n"
	res, isErr, rpcErr := srv.callTool(nil, "validate_pipeline", map[string]interface{}{"yaml": yamlStr})
	if rpcErr != nil || isErr {
		t.Fatalf("relative plugin failed: rpc=%v isErr=%v result=%s", rpcErr, isErr, res)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("relative plugin validation failed: %s", res)
	}
}

func TestMCPRejectsFileRefOutsideWorkdir(t *testing.T) {
	srv := testServer(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	writePolicyPlugin(t, srv.pluginsDirs[0], "filereader", nil, map[string]interface{}{
		"path": map[string]interface{}{"from": "input.path", "type": "string", "format": "file_ref"},
	})
	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: file_read\n  input:\n    path: " + outside + "\n  steps:\n    - id: read\n      plugin: " + filepath.Join(srv.pluginsDirs[0], "filereader") + "\n      bind:\n        path: input.path\n"
	// Отказ политики — результат проверки (ok:false + issue), а не RPC-ошибка;
	// run_pipeline при этом всё равно отказывает.
	assertPolicyRefusal(t, srv, yamlStr, "E_FILE_REF_OUTSIDE_ROOT", "E_FILE_REF_OUTSIDE_ROOT")
}

func TestMCPAllowsFileRefInsideWorkdir(t *testing.T) {
	srv := testServer(t)
	inside := filepath.Join(srv.workDir, "inside.txt")
	if err := os.WriteFile(inside, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	pluginDir := writePolicyPlugin(t, srv.pluginsDirs[0], "filereader", nil, map[string]interface{}{
		"path": map[string]interface{}{"from": "input.path", "type": "string", "format": "file_ref"},
	})
	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: file_read\n  input:\n    path: " + inside + "\n  steps:\n    - id: read\n      plugin: " + pluginDir + "\n      bind:\n        path: input.path\n"
	res, _, rpcErr := srv.callTool(nil, "validate_pipeline", map[string]interface{}{"yaml": yamlStr})
	if rpcErr != nil {
		t.Fatalf("inside file_ref rejected: %v", rpcErr)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res), &out); err != nil || !out.OK {
		t.Fatalf("inside file_ref validation: %s", res)
	}
}

func TestMCPRejectsDynamicForeachFileRef(t *testing.T) {
	srv := testServer(t)
	inside := filepath.Join(srv.workDir, "inside.txt")
	if err := os.WriteFile(inside, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	pluginDir := writePolicyPlugin(t, srv.pluginsDirs[0], "filereader", nil, map[string]interface{}{
		"path": map[string]interface{}{"from": "input.path", "type": "string", "format": "file_ref"},
	})
	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: dynamic_file_read\n  input:\n    paths:\n      - " + strconv.Quote(inside) + "\n  foreach: input.paths\n  foreach_item: path\n  steps:\n    - id: read\n      plugin: " + strconv.Quote(pluginDir) + "\n      bind:\n        path: input.path\n"
	_, _, rpcErr := srv.callTool(nil, "validate_pipeline", map[string]interface{}{"yaml": yamlStr})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "E_FILE_REF_UNCHECKED") {
		t.Fatalf("dynamic file_ref was accepted: %v", rpcErr)
	}
}

func TestMCPRejectsForeachItemLeakIntoFileRef(t *testing.T) {
	srv := testServer(t)
	inside := filepath.Join(srv.workDir, "inside.txt")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(inside, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	pluginDir := writePolicyPlugin(t, srv.pluginsDirs[0], "filereader", nil, map[string]interface{}{
		"path": map[string]interface{}{"from": "input.path", "type": "string", "format": "file_ref"},
	})
	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: foreach_leak\n  input:\n    path: " + strconv.Quote(inside) + "\n    paths:\n      - " + strconv.Quote(outside) + "\n  steps:\n    - id: iterate\n      plugin: " + strconv.Quote(filepath.Join(srv.pluginsDirs[0], "echoer")) + "\n      foreach: input.paths\n      foreach_item: path\n      bind:\n        text: input.path\n    - id: read\n      plugin: " + strconv.Quote(pluginDir) + "\n      bind:\n        path: input.path\n"
	_, _, rpcErr := srv.callTool(nil, "validate_pipeline", map[string]interface{}{"yaml": yamlStr})
	if rpcErr == nil || !strings.Contains(rpcErr.Message, "E_FILE_REF_UNCHECKED") {
		t.Fatalf("leaked foreach file_ref was accepted: %v", rpcErr)
	}
}

func TestMCPRejectsPluginSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	plugins := filepath.Join(work, "plugins")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(plugins, 0755); err != nil {
		t.Fatal(err)
	}
	writeFakePlugin(t, outside, "escape",
		map[string]interface{}{"text": map[string]interface{}{"type": "string", "from": "input.text"}},
		map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}})
	link := filepath.Join(plugins, "escape")
	if err := os.Symlink(filepath.Join(outside, "escape"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	srv, err := NewServer(Options{PluginsDirs: []string{plugins}, WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: symlink_escape\n  input:\n    text: hello\n  steps:\n    - id: s\n      plugin: " + strconv.Quote(filepath.Join("plugins", "escape")) + "\n      bind:\n        text: input.text\n"
	// Symlink-обход — тот же отказ политики: ok:false + issue, run отказывает.
	assertPolicyRefusal(t, srv, yamlStr, "E_PLUGIN_OUTSIDE_ROOT", "E_PLUGIN_OUTSIDE_ROOT")
}

func TestMCPCancelRun(t *testing.T) {
	srv := testServer(t)
	// sleeper-плагин
	dir := t.TempDir()
	plugDir := dir + "/plugins"
	_ = plugDir
	sleeperSrc := srv.pluginsDirs[0] + "/sleeper"
	os.MkdirAll(sleeperSrc, 0755)
	manifest := map[string]interface{}{
		"id": "sleeper", "version": "0.1", "platform_api": "0.1",
		"runtime": map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":   map[string]interface{}{},
		"output":  map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}},
	}
	raw, _ := json.Marshal(manifest)
	os.WriteFile(sleeperSrc+"/plugin.yaml", raw, 0644)
	py := "import sys,json,time\njson.load(sys.stdin)\ntime.sleep(30)\njson.dump({'status':'ok','output':{'done':True}},sys.stdout)\n"
	os.WriteFile(sleeperSrc+"/main.py", []byte(py), 0644)
	// sleeper добавлен ПОСЛЕ сборки сервера, поэтому доверие пересчитываем:
	// иначе плагин остался бы внешним кодом и ран упал бы мгновенно, а
	// отменять было бы нечего.
	srv.trust.Trusted = trustDirsIn(t, srv.pluginsDirs[0])
	yamlStr := "format_version: \"0.2\"\npipeline:\n  name: sleep_demo\n  input: {}\n  steps:\n    - id: sleep\n      plugin: " + sleeperSrc + "\n      timeout: 30s\n"
	res, _, rpcErr := srv.callTool(nil, "run_pipeline", map[string]interface{}{"yaml": yamlStr})
	if rpcErr != nil {
		t.Fatalf("run: %v", rpcErr)
	}
	var runOut struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(res), &runOut)
	time.Sleep(500 * time.Millisecond)
	cancelRes, _, rpcErr := srv.callTool(nil, "cancel_run", map[string]interface{}{"run_id": runOut.RunID})
	if rpcErr != nil {
		t.Fatalf("cancel: %v", rpcErr)
	}
	if !strings.Contains(cancelRes, "cancel") && !strings.Contains(cancelRes, "cancelled") {
		t.Errorf("cancel_run вернул неожиданное: %q", cancelRes)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		res2, _, _ := srv.callTool(nil, "get_run", map[string]interface{}{"run_id": runOut.RunID})
		var g struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal([]byte(res2), &g)
		if g.Status == "cancelled" {
			return
		}
		if g.Status == "failed" {
			t.Fatalf("failed вместо cancelled: %s", res2)
		}
		if time.Now().After(deadline) {
			t.Fatalf("нет cancelled: %s", res2)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestMCPServeHandlesCancelWhileRunWaits(t *testing.T) {
	srv := testServer(t)
	plugDir := filepath.Join(srv.pluginsDirs[0], "sleeper")
	if err := os.MkdirAll(plugDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]interface{}{
		"id": "sleeper", "version": "0.1", "platform_api": "0.1",
		"runtime": map[string]interface{}{"type": "python", "entry": "main.py"},
		"input":   map[string]interface{}{},
		"output":  map[string]interface{}{"done": map[string]interface{}{"type": "boolean"}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(plugDir, "plugin.yaml"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	py := "import sys,json,time\njson.load(sys.stdin)\ntime.sleep(30)\njson.dump({'status':'ok','output':{'done':True}},sys.stdout)\n"
	if err := os.WriteFile(filepath.Join(plugDir, "main.py"), []byte(py), 0644); err != nil {
		t.Fatal(err)
	}
	// sleeper создан ПОСЛЕ testServer(), который посчитал allow-list, поэтому
	// плагина в нём не было и он остался недоверенным: без изолятора на этой
	// платформе ран падал сразу, не выполнив ни одного шага.
	//
	// Тест это пропускал случайно, а не потому что был прав. currentRunID
	// ставится ДО исполнения, и сразу после падения сбрасывается в ""; тест
	// опрашивает его каждые 20 мс, поэтому локально успевал зацепить это
	// короткое окно и пройти, а на CI (медленнее) — нет. Раньше это выглядело
	// как «иногда тест падает», теперь ран реально живёт 30 секунд.
	srv.trust.Trusted = trustDirsIn(t, srv.pluginsDirs[0])
	yamlText := "format_version: \"0.2\"\npipeline:\n  name: concurrent_cancel\n  input: {}\n  steps:\n    - id: sleep\n      plugin: " + strconv.Quote(plugDir) + "\n      timeout: 30s\n"
	runArgs := map[string]interface{}{"name": "run_pipeline", "arguments": map[string]interface{}{"yaml": yamlText, "wait_seconds": 10.0}}
	runReq := Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call", Params: mustJSON(t, runArgs)}
	cancelArgs := map[string]interface{}{"name": "cancel_run", "arguments": map[string]interface{}{}}
	cancelReq := Request{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/call", Params: mustJSON(t, cancelArgs)}

	in, inWriter := io.Pipe()
	var out bytes.Buffer
	transport := NewTransport(in, &out)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(transport) }()
	if _, err := inWriter.Write(append(mustJSON(t, runReq), '\n')); err != nil {
		t.Fatal(err)
	}

	var runID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		runID = srv.currentRunID
		_, hasCancel := srv.cancels[runID]
		srv.mu.Unlock()
		if runID != "" && hasCancel {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("run did not start")
	}
	cancelArgs["arguments"] = map[string]interface{}{"run_id": runID}
	cancelReq.Params = mustJSON(t, cancelArgs)
	if _, err := inWriter.Write(append(mustJSON(t, cancelReq), '\n')); err != nil {
		t.Fatal(err)
	}
	_ = inWriter.Close()
	select {
	case err := <-done:
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not finish after cancellation")
	}
	responses := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(responses) < 2 || !strings.Contains(out.String(), "cancelled") {
		t.Fatalf("responses=%s", out.String())
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Без пайпов: handle() возвращает только Response, runner/gate в MCP идут
// с Quiet:true и os.Stdout→stderr (см. cli/mcp.go), плагины пишут в capture-буферы.
func TestMCPStdioIsolation(t *testing.T) {
	srv := testServer(t)
	mkReq := func(id int64, method string, params interface{}) *Request {
		raw, _ := json.Marshal(params)
		rawID, _ := json.Marshal(id)
		return &Request{JSONRPC: "2.0", ID: rawID, Method: method, Params: raw}
	}
	reqs := []*Request{
		mkReq(1, "initialize", map[string]interface{}{}),
		mkReq(2, "tools/list", map[string]interface{}{}),
		mkReq(3, "tools/call", map[string]interface{}{"name": "list_plugins", "arguments": map[string]interface{}{}}),
	}
	for _, req := range reqs {
		resp := srv.handle(req)
		b, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		var back Response
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("ответ не JSON-RPC: %q", string(b))
		}
		if back.JSONRPC != "" && back.JSONRPC != "2.0" {
			t.Fatalf("bad jsonrpc: %q", string(b))
		}
	}
}
