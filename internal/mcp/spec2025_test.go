package mcp

// Тесты контракта ревизии 2025-06-18: structuredContent, outputSchema,
// notifications/progress и elicitation/create. Каждый тест называет пункт
// спеки, который проверяет, — чтобы «зелёный» означал соответствие, а не
// отсутствие ошибок.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// --- клиент, который умеет отвечать серверу ---

// fakeClient — MCP-клиент поверх настоящих каналов: читает всё, что сервер
// пишет, и отвечает на серверные запросы. Нужен ровно потому, что направление
// «сервер → клиент» появилось только с elicitation: без ответов клиента этот
// путь нельзя проверить ничем, кроме настоящего обмена.
type fakeClient struct {
	t       *testing.T
	w       *io.PipeWriter
	mu      sync.Mutex
	seen    []map[string]interface{}
	answers map[string]func(params map[string]interface{}) map[string]interface{}
	done    chan struct{}
}

func startFakeClient(t *testing.T, r *io.PipeReader, w *io.PipeWriter) *fakeClient {
	t.Helper()
	c := &fakeClient{t: t, w: w, answers: map[string]func(map[string]interface{}) map[string]interface{}{}, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var msg map[string]interface{}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			c.mu.Lock()
			c.seen = append(c.seen, msg)
			c.mu.Unlock()
			// Серверный запрос: есть method и id. Ответ на него — наша работа.
			if _, hasMethod := msg["method"]; !hasMethod {
				continue
			}
			id, _ := msg["id"].(string)
			if id == "" && msg["id"] == nil {
				continue
			}
			method, _ := msg["method"].(string)
			params, _ := msg["params"].(map[string]interface{})
			c.mu.Lock()
			answer := c.answers[method]
			c.mu.Unlock()
			if answer == nil {
				continue
			}
			result := answer(params)
			resp := map[string]interface{}{"jsonrpc": "2.0", "id": msg["id"]}
			if result == nil {
				resp["error"] = map[string]interface{}{"code": -32601, "message": "нет ответа для " + method}
			} else {
				resp["result"] = result
			}
			raw, _ := json.Marshal(resp)
			_, _ = c.w.Write(append(raw, '\n'))
		}
	}()
	return c
}

func (c *fakeClient) on(method string, f func(map[string]interface{}) map[string]interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers[method] = f
}

func (c *fakeClient) send(v interface{}) {
	c.t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.w.Write(append(raw, '\n')); err != nil {
		c.t.Fatalf("клиент не смог отправить сообщение: %v", err)
	}
}

// call посылает tools/call и ждёт ответ с этим id.
func (c *fakeClient) call(id int, name string, args map[string]interface{}, meta map[string]interface{}) map[string]interface{} {
	c.t.Helper()
	params := map[string]interface{}{"name": name, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	c.send(map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if msg := c.response(id); msg != nil {
			return msg
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("ответ на вызов %s не пришёл", name)
	return nil
}

func (c *fakeClient) response(id int) map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, msg := range c.seen {
		if _, hasMethod := msg["method"]; hasMethod {
			continue
		}
		if got, ok := msg["id"].(float64); ok && int(got) == id {
			return msg
		}
	}
	return nil
}

// notifications возвращает всё, что сервер отправил как нотификацию.
func (c *fakeClient) notifications(method string) []map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]interface{}
	for _, msg := range c.seen {
		if m, _ := msg["method"].(string); m == method {
			out = append(out, msg)
		}
	}
	return out
}

// session — поднятый сервер с фейковым клиентом.
type session struct {
	srv    *Server
	client *fakeClient
	stop   func()
}

// startSession — сервер и клиент на двух каналах; clientSetup правит клиента
// (например, добавляет ответ на elicitation) до первого обмена.
func startSession(t *testing.T, srv *Server, clientSetup func(*fakeClient)) *session {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	tr := NewTransport(serverIn, serverOut)
	go func() { _ = srv.Serve(tr) }()
	client := startFakeClient(t, clientIn, clientOut)
	if clientSetup != nil {
		clientSetup(client)
	}
	stop := func() {
		_ = clientOut.Close()
		_ = serverOut.Close()
		_ = clientIn.Close()
		_ = serverIn.Close()
	}
	return &session{srv: srv, client: client, stop: stop}
}

func (s *session) initialize(t *testing.T, capabilities map[string]interface{}) {
	t.Helper()
	s.client.send(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2025-06-18",
			"capabilities":    capabilities,
			"clientInfo":      map[string]interface{}{"name": "spec-cli", "version": "1"},
		},
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.client.response(1) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("initialize не ответил")
}

// gatePipeline — пайплайн, который доходит до гейта и не требует сети.
func gatePipeline(t *testing.T, srv *Server) string {
	t.Helper()
	pluginDir := srv.pluginsDirs[0] + "/echoer"
	return "format_version: \"0.2\"\npipeline:\n  name: elicit_gate\n  input:\n    text: hello\n  steps:\n" +
		"    - id: echo\n      plugin: " + strconv.Quote(pluginDir) + "\n" +
		"      bind:\n        text: input.text\n" +
		"    - id: review\n      plugin: core/human_gate\n" +
		"      form:\n        - { field: steps.echo.done, editable: false }\n" +
		"      actions: [accept, reject]\n      on_reject: stop\n"
}

func resultOf(t *testing.T, resp map[string]interface{}) map[string]interface{} {
	t.Helper()
	res, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("нет result в ответе: %v", resp)
	}
	return res
}

func textPayload(t *testing.T, resp map[string]interface{}) map[string]interface{} {
	t.Helper()
	res := resultOf(t, resp)
	content, ok := res["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("нет content: %v", res)
	}
	first := content[0].(map[string]interface{})
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(first["text"].(string)), &out); err != nil {
		t.Fatalf("текстовый блок не JSON: %v", err)
	}
	return out
}

// waitStatus ждёт, пока ран придёт в нужный статус.
func waitStatus(t *testing.T, srv *Server, runID string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status := srv.runStatus(runID)
		for _, w := range want {
			if status == w {
				return status
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return srv.runStatus(runID)
}

// --- 1. structuredContent и outputSchema (2025-06-18, смена tools/call) ---

func TestConformanceStructuredContentMatchesSchema(t *testing.T) {
	srv := testServer(t)
	resp := srv.handle(&Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: json.RawMessage(`{"name":"list_plugins","arguments":{}}`)})
	res := resultOfRaw(t, resp)
	if _, ok := res["structuredContent"]; !ok {
		t.Fatalf("нет structuredContent: %v", res)
	}
	// Текстовый блок обязан остаться: спека требует его для совместимости.
	content := res["content"].([]map[string]interface{})
	if content[0]["type"] != "text" {
		t.Fatalf("текстовый блок пропал: %v", content)
	}
	if err := conformsTo(schemaOf(t, "list_plugins"), res["structuredContent"]); err != nil {
		t.Fatalf("structuredContent не соответствует outputSchema: %v", err)
	}
}

func TestConformanceStructuredContentOnIssues(t *testing.T) {
	srv := testServer(t)
	resp := srv.handle(&Request{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/call",
		Params: json.RawMessage(`{"name":"validate_pipeline","arguments":{"yaml":"format_version: \"0.2\"\npipeline:\n  name: x\n  input: {}\n  steps: []\n"}}`)})
	res := resultOfRaw(t, resp)
	sc, ok := res["structuredContent"].(map[string]interface{})
	if !ok {
		t.Fatalf("нет structuredContent: %v", res)
	}
	if _, ok := sc["ok"].(bool); !ok {
		t.Fatalf("в structuredContent нет ok: %v", sc)
	}
	if err := conformsTo(schemaOf(t, "validate_pipeline"), sc); err != nil {
		t.Fatalf("не соответствует схеме: %v", err)
	}
}

func TestConformanceEveryToolHasOutputSchema(t *testing.T) {
	for _, def := range toolDefs() {
		if def.OutputSchema == nil {
			t.Fatalf("%s: нет outputSchema", def.Name)
		}
		if def.OutputSchema["type"] != "object" {
			t.Fatalf("%s: outputSchema не объект: %v", def.Name, def.OutputSchema["type"])
		}
		if _, ok := def.OutputSchema["properties"]; !ok {
			t.Fatalf("%s: outputSchema без properties", def.Name)
		}
	}
	// run_pipeline не имеет права объявлять run_id обязательным: при отказе
	// валидации он возвращает {ok, issues} без run_id.
	if req, ok := schemaOf(t, "run_pipeline")["required"].([]string); ok {
		for _, name := range req {
			if name == "run_id" {
				t.Fatal("run_pipeline: run_id объявлен обязательным, но его нет при отказе валидации")
			}
		}
	}
}

func TestConformanceNoStructuredContentOnError(t *testing.T) {
	srv := testServer(t)
	resp := srv.handle(&Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: json.RawMessage(`{"name":"describe_plugin","arguments":{"id":"нет-такого"}}`)})
	if resp.Error == nil {
		t.Fatal("ожидалась ошибка")
	}
	if resp.Result != nil {
		t.Fatalf("при ошибке результата быть не должно: %v", resp.Result)
	}
}

// --- 2. notifications/progress ---

func TestConformanceProgressRequiresToken(t *testing.T) {
	srv := testServer(t)
	out := &lockedBuffer{}
	tr := NewTransport(strings.NewReader(""), out)
	call := srv.beginCall(tr, &Request{ID: json.RawMessage(`7`)})
	srv.notifyProgress(call, 1, 10, "без токена")
	if strings.Contains(out.String(), "progress") {
		t.Fatalf("прогресс ушёл без progressToken: %s", out.String())
	}
	call.progressToken = json.RawMessage(`"tok-1"`)
	srv.notifyProgress(call, 1, 10, "с токеном")
	if !strings.Contains(out.String(), `"method":"notifications/progress"`) {
		t.Fatalf("прогресс не отправлен: %s", out.String())
	}
	var msg map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &msg); err != nil {
		t.Fatal(err)
	}
	params := msg["params"].(map[string]interface{})
	if params["progressToken"] != "tok-1" || params["progress"].(float64) != 1 || params["total"].(float64) != 10 {
		t.Fatalf("прогресс собран неверно: %v", params)
	}
}

func TestConformanceProgressWhileWaiting(t *testing.T) {
	srv := testServer(t)
	// Долгий плагин без гейта: ран живёт и без канала к человеку (гейт здесь
	// не нужен), а ожидание идёт больше секунды — значит прогресс обязан
	// отметиться несколько раз, а не только на входе и выходе.
	plugDir := writeFakePluginDir(t, srv.pluginsDirs[0], "slowpoke",
		"import sys,json,time\njson.load(sys.stdin)\ntime.sleep(2.5)\nprint('{\"status\":\"ok\",\"output\":{}}')\n")
	srv.trust.Trusted = trustDirsIn(t, srv.pluginsDirs[0])
	yaml := "format_version: \"0.2\"\npipeline:\n  name: progress_probe\n  input: {}\n  steps:\n" +
		"    - id: slow\n      plugin: " + strconv.Quote(plugDir) + "\n      timeout: 30s\n"

	sess := startSession(t, srv, nil)
	defer sess.stop()
	sess.initialize(t, map[string]interface{}{})

	sess.client.call(2, "run_pipeline", map[string]interface{}{
		"yaml": yaml, "wait_seconds": 6.0,
	}, map[string]interface{}{"progressToken": "tok-42"})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(sess.client.notifications("notifications/progress")) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	notifs := sess.client.notifications("notifications/progress")
	if len(notifs) < 2 {
		t.Fatalf("прогресс не шёл во время ожидания: %v", notifs)
	}
	params := notifs[0]["params"].(map[string]interface{})
	if params["progressToken"] != "tok-42" {
		t.Fatalf("токен прогресса потерян: %v", params)
	}
	// Прогресс обязан расти, а не стоять на месте: иначе клиент не отличит
	// «работа идёт» от «сервер завис».
	last := notifs[len(notifs)-1]["params"].(map[string]interface{})
	if last["progress"].(float64) <= params["progress"].(float64) && len(notifs) > 1 {
		t.Fatalf("прогресс не увеличивается: %v", notifs)
	}
}

// --- 3. elicitation/create ---

func elicitSession(t *testing.T, action string, content map[string]interface{}) *session {
	t.Helper()
	srv := testServer(t)
	srv.gateElicitation = true
	sess := startSession(t, srv, func(c *fakeClient) {
		c.on("elicitation/create", func(map[string]interface{}) map[string]interface{} {
			if action == "" {
				return nil // клиент отвечает ошибкой
			}
			return map[string]interface{}{"action": action, "content": content}
		})
	})
	sess.initialize(t, map[string]interface{}{"elicitation": map[string]interface{}{}})
	return sess
}

func TestConformanceElicitationAccept(t *testing.T) {
	sess := elicitSession(t, "accept", nil)
	defer sess.stop()
	resp := sess.client.call(2, "run_pipeline", map[string]interface{}{
		"yaml": gatePipeline(t, sess.srv), "wait_seconds": 15.0,
	}, nil)
	payload := textPayload(t, resp)
	runID := payload["run_id"].(string)
	if status := waitStatus(t, sess.srv, runID, "done", "failed"); status != "done" {
		t.Fatalf("ран не завершился успешно: %s (%v)", status, payload)
	}
	// Журнал обязан назвать источник решения: диалог клиента — не консоль.
	dir, _ := journal.SafeRunDir(sess.srv.runsDir, runID)
	res, err := journal.NewReader(dir).EventsBounded(0, runJournalLimits(false))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res.Events {
		if e["type"] == "gate_decision" {
			if e["source"] != "mcp_elicitation" {
				t.Fatalf("источник решения %v, ожидался mcp_elicitation", e["source"])
			}
			if e["action"] != "accept" {
				t.Fatalf("действие %v, ожидался accept", e["action"])
			}
			found = true
		}
	}
	if !found {
		t.Fatal("в журнале нет решения гейта")
	}
}

func TestConformanceElicitationDecline(t *testing.T) {
	sess := elicitSession(t, "decline", nil)
	defer sess.stop()
	resp := sess.client.call(2, "run_pipeline", map[string]interface{}{
		"yaml": gatePipeline(t, sess.srv), "wait_seconds": 15.0,
	}, nil)
	payload := textPayload(t, resp)
	runID := payload["run_id"].(string)
	// on_reject: stop — ран обязан остановиться, а не продолжиться молча.
	if status := waitStatus(t, sess.srv, runID, "failed", "done"); status != "failed" {
		t.Fatalf("отказ человека не остановил ран: %s", status)
	}
}

func TestConformanceElicitationCancelIsStop(t *testing.T) {
	sess := elicitSession(t, "cancel", nil)
	defer sess.stop()
	resp := sess.client.call(2, "run_pipeline", map[string]interface{}{
		"yaml": gatePipeline(t, sess.srv), "wait_seconds": 15.0,
	}, nil)
	payload := textPayload(t, resp)
	runID := payload["run_id"].(string)
	if status := waitStatus(t, sess.srv, runID, "failed", "done"); status != "failed" {
		t.Fatalf("отмена диалога не остановила ран: %s", status)
	}
}

func TestConformanceElicitationEditsApplied(t *testing.T) {
	// Правка формы из диалога обязана дойти до контекста рана: значение поля
	// приходит строкой, но гейт принимает его как значение нужного типа.
	srv := testServer(t)
	srv.gateElicitation = true
	pluginDir := srv.pluginsDirs[0] + "/echoer"
	// Поле editable: правка — то, ради чего elicitation и нужен.
	yaml := "format_version: \"0.2\"\npipeline:\n  name: elicit_edit\n  input:\n    text: hello\n  steps:\n" +
		"    - id: echo\n      plugin: " + strconv.Quote(pluginDir) + "\n      bind:\n        text: input.text\n" +
		"    - id: review\n      plugin: core/human_gate\n" +
		"      form:\n        - { field: steps.echo.done, editable: true }\n" +
		"      actions: [accept, reject]\n"
	sess := startSession(t, srv, func(c *fakeClient) {
		c.on("elicitation/create", func(params map[string]interface{}) map[string]interface{} {
			schema, _ := params["requestedSchema"].(map[string]interface{})
			props, _ := schema["properties"].(map[string]interface{})
			content := map[string]interface{}{}
			for name := range props {
				content[name] = "true" // булево значение строкой, как гнёт схема
			}
			return map[string]interface{}{"action": "accept", "content": content}
		})
	})
	defer sess.stop()
	sess.initialize(t, map[string]interface{}{"elicitation": map[string]interface{}{}})
	resp := sess.client.call(2, "run_pipeline", map[string]interface{}{"yaml": yaml, "wait_seconds": 15.0}, nil)
	payload := textPayload(t, resp)
	runID := payload["run_id"].(string)
	if status := waitStatus(t, srv, runID, "done", "failed"); status != "done" {
		t.Fatalf("ран с правкой не завершился: %s", status)
	}
	dir, _ := journal.SafeRunDir(srv.runsDir, runID)
	res, _ := journal.NewReader(dir).EventsBounded(0, runJournalLimits(false))
	edited := false
	for _, e := range res.Events {
		if e["type"] != "gate_decision" {
			continue
		}
		edits, _ := e["edits"].(map[string]interface{})
		for _, v := range edits {
			// "true" разобран как JSON → bool, а не строка.
			if _, ok := v.(bool); ok {
				edited = true
			}
		}
	}
	if !edited {
		t.Fatal("правка из диалога не дошла до контекста (ожидался bool)")
	}
}

// Клиент без elicitation + без консоли: ран обязан быть отклонён до старта,
// как и раньше. Появление нового канала не должно ослаблять старый отказ.
func TestConformanceGateWithoutChannelRefused(t *testing.T) {
	srv := testServer(t)
	_, _, rpcErr := srv.callTool(nil, "run_pipeline", map[string]interface{}{
		"yaml": gatePipeline(t, srv), "wait_seconds": 1.0})
	if rpcErr == nil || rpcErr.Data == nil {
		t.Fatalf("ожидался отказ: %+v", rpcErr)
	}
	if m, _ := rpcErr.Data.(map[string]string); m["code"] != "E_NO_HUMAN_CHANNEL" {
		t.Fatalf("ожидался E_NO_HUMAN_CHANNEL, получено %+v", rpcErr.Data)
	}
}

// Флаг есть, но клиент не объявил elicitation — отказ тот же.
func TestConformanceElicitationNeedsCapability(t *testing.T) {
	srv := testServer(t)
	srv.gateElicitation = true
	_, _, rpcErr := srv.callTool(nil, "run_pipeline", map[string]interface{}{
		"yaml": gatePipeline(t, srv), "wait_seconds": 1.0})
	if rpcErr == nil || rpcErr.Data == nil {
		t.Fatalf("ожидался отказ: %+v", rpcErr)
	}
	if m, _ := rpcErr.Data.(map[string]string); m["code"] != "E_NO_HUMAN_CHANNEL" {
		t.Fatalf("ожидался E_NO_HUMAN_CHANNEL, получено %+v", rpcErr.Data)
	}
}

// Флаг выключен, клиент умеет elicitation — сервер обязан остаться на консоли
// (или на отказе): доверие каналу выдаёт оператор, а не клиент.
func TestConformanceElicitationOffByDefault(t *testing.T) {
	srv := testServer(t)
	_, _, rpcErr := srv.callTool(nil, "run_pipeline", map[string]interface{}{
		"yaml": gatePipeline(t, srv), "wait_seconds": 1.0})
	if rpcErr == nil {
		t.Fatal("гейт через elicitation прошёл без флага оператора")
	}
}

func TestConformanceClientCapabilities(t *testing.T) {
	srv := testServer(t)
	srv.handle(&Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{"elicitation":{}}}`)})
	if !srv.clientElicitation {
		t.Fatal("возможность elicitation не распознана")
	}
	srv.handle(&Request{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "initialize",
		Params: json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}`)})
	if srv.clientElicitation {
		t.Fatal("elicitation не объявлен, но помечен как поддержанный")
	}
}

// Серверный запрос и ответ на него: классификация сообщений по признаку method.
func TestConformanceClientResponseClassified(t *testing.T) {
	item := parseItem([]byte(`{"jsonrpc":"2.0","id":"fidespipe-1","result":{"action":"accept"}}`))
	if item.Resp == nil {
		t.Fatalf("ответ клиента не распознан: %+v", item)
	}
	if item.Request != nil {
		t.Fatal("ответ клиента принят за запрос")
	}
	// Ни method, ни id — не сообщение JSON-RPC: это ошибка, а не запрос.
	item = parseItem([]byte(`{"jsonrpc":"2.0"}`))
	if item.Err == nil {
		t.Fatalf("сообщение без method и id обязано быть ошибкой: %+v", item)
	}
}

func TestConformanceUnknownClientResponseIgnored(t *testing.T) {
	srv := testServer(t)
	// Ответ на запрос, которого никто не ждал: не паника, не зависание.
	srv.deliverClientResponse(&clientResponse{ID: json.RawMessage(`"fidespipe-нет"`)})
	if len(srv.pending) != 0 {
		t.Fatalf("карта ожиданий не пуста: %v", srv.pending)
	}
}

func TestConformanceElicitationTimeoutIsStop(t *testing.T) {
	// Клиент не отвечает вовсе: контекст истекает, решение не выдумывается.
	srv := testServer(t)
	srv.gateElicitation = true
	srv.clientElicitation = true
	ui := &elicitGateUI{srv: srv, step: gateStepForTest(t, srv), runID: "нет-такого"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := srv.sendClientRequest(ctx, "elicitation/create", map[string]interface{}{})
	if err == nil {
		t.Fatal("ожидался таймаут без транспорта")
	}
	if _, derr := ui.WaitDecision(); derr == nil {
		t.Fatal("без ответа клиента решение приниматься не должно")
	}
}

// --- служебное ---

func resultOfRaw(t *testing.T, resp *Response) map[string]interface{} {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("неожиданная ошибка: %+v", resp.Error)
	}
	res, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("результат не объект: %#v", resp.Result)
	}
	return res
}

func schemaOf(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	for _, def := range toolDefs() {
		if def.Name == name {
			return def.OutputSchema
		}
	}
	t.Fatalf("инструмент %s не найден", name)
	return nil
}

func gateStepForTest(t *testing.T, srv *Server) *pipeline.Step {
	t.Helper()
	pf, err := pipeline.LoadPipelineFileFromBytes([]byte(gatePipeline(t, srv)))
	if err != nil {
		t.Fatal(err)
	}
	for i := range pf.Pipeline.Steps {
		if strings.HasPrefix(pf.Pipeline.Steps[i].Plugin, "core/") {
			return &pf.Pipeline.Steps[i]
		}
	}
	t.Fatal("в пайплайне нет шага-гейта")
	return nil
}

func conformsTo(schema map[string]interface{}, value interface{}) error {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return fmt.Errorf("значение не объект: %T", value)
	}
	props, _ := schema["properties"].(map[string]interface{})
	for name, raw := range props {
		sch, _ := raw.(map[string]interface{})
		want, _ := sch["type"].(string)
		v, present := obj[name]
		if !present {
			continue
		}
		bad := false
		switch want {
		case "string":
			_, bad = v.(string)
		case "boolean":
			_, bad = v.(bool)
		case "number":
			_, bad = v.(float64)
		case "object":
			_, bad = v.(map[string]interface{})
		case "array":
			_, bad = v.([]interface{})
		default:
			continue
		}
		if !bad {
			return fmt.Errorf("поле %s: ожидался %s, получено %T", name, want, v)
		}
	}
	if req, ok := schema["required"].([]string); ok {
		for _, name := range req {
			if _, present := obj[name]; !present {
				return fmt.Errorf("нет обязательного поля %s", name)
			}
		}
	}
	return nil
}

// Отмена рана обязана прерывать ожидание диалога: клиент, который никогда не
// ответит, не должен держать ран «отменённым на бумаге» до получасового
// таймаута. Здесь клиент намеренно не отвечает на elicitation.
func TestConformanceElicitationWaitInterruptedByCancel(t *testing.T) {
	srv := testServer(t)
	srv.gateElicitation = true
	sess := startSession(t, srv, func(c *fakeClient) {
		// Ответа нет вовсе: сервер обязан выйти из ожидания сам.
	})
	defer sess.stop()
	sess.initialize(t, map[string]interface{}{"elicitation": map[string]interface{}{}})

	sess.client.send(map[string]interface{}{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]interface{}{"name": "run_pipeline",
			"arguments": map[string]interface{}{"yaml": gatePipeline(t, srv), "wait_seconds": 60.0}}})
	// Ждём, пока ран дойдёт до гейта и повиснет на диалоге.
	var runID string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		runID = srv.currentRunID
		srv.mu.Unlock()
		if runID != "" && srv.runStatus(runID) == "waiting_human" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("ран не стартовал")
	}
	// Отменяем ран инструментом: ожидание диалога обязано прерваться.
	sess.client.call(8, "cancel_run", map[string]interface{}{"run_id": runID}, nil)
	if status := waitStatus(t, srv, runID, "cancelled", "failed"); status != "cancelled" {
		t.Fatalf("отменённый ран остался в статусе %s", status)
	}
}
