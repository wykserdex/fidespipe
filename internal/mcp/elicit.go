package mcp

// Гейт человека через elicitation/create (ревизия 2025-06-18).
//
// Зачем это, если у проекта уже есть консоль гейтов в браузере. Консоль —
// собственная вселенная: она показывает форму человеку, но хостед-клиент о
// гейте не знает, а без GUI (--no-gui) гейт вообще отклоняется
// (E_NO_HUMAN_CHANNEL). elicitation — стандартный способ, которым сервер
// спрашивает человека через интерфейс клиента, и он поднимает гейт там, где
// человек уже находится.
//
// Границы, названные честно:
//   - Направление «сервер → клиент» появилось в 2025-06-18; на старой ревизии
//     этого пути нет, поэтому гейт уходит в консоль или отклоняется.
//   - Схема elicitation плоская: только примитивы. Поля формы гейта — тоже
//     плоские строки, поэтому отображение честное, но вложенных структур
//     (массивов объектов) в гейте через elicitation не покажешь.
//   - fidespipe НЕ МОЖЕТ проверить, что за клиентом сидит человек: автономный хост
//     вправе ответить сам. Именно поэтому путь не включается сам собой, а
//     требует флага оператора --gate-elicitation: решение человека — это
//     решение человека, и объявлять его таковым может только тот, кто знает
//     своего клиента.
//   - Решение по-прежнему недоступно агенту как инструмент: агент не имеет
//     вызова «одобрить». Он может лишь передать запрос человека, а показывать
//     или не показывать диалог решает хост клиента.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wykserdex/fidespipe/internal/gate"
	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// elicitTimeout — сколько ждать ответа человека. Гейт — это не запрос к API:
// человек может уйти на обед. Таймаут здесь только чтобы не висеть вечно при
// мёртвом клиенте; истечение — стоп (EOF-семантика), а не «accept».
const elicitTimeout = 30 * time.Minute

// elicitGateUI — gate.GateUI + gate.StructuredUI поверх elicitation/create.
//
// ReadLine возвращает EOF по той же причине, что и у ChannelUI: построчный
// режим предполагает печать промптов в чужой терминал, которого здесь нет.
// Гейт идёт структурированным путём (runStructured).
type elicitGateUI struct {
	srv   *Server
	call  *inflightCall
	runID string
	step  *pipeline.Step
	// runCtx — контекст рана. Ожидание диалога обязано прерываться вместе с
	// раном: иначе cancel_run (или отмена человеком в консоли) оставляла бы
	// гейт висеть до таймаута, а ран — «отменённым» только на бумаге.
	runCtx context.Context
	runner *elicitRunner
}

// elicitRunner — общий для рана: держит фолбэк на консоль и признак «мы уже
// уходили в фолбэк», чтобы после первой неудачи не пытаться снова и не
// показывать человеку два интерфейса на один гейт.
type elicitRunner struct {
	mu       sync.Mutex
	fallback gate.StructuredUI
	used     bool
}

func (u *elicitGateUI) ReadLine() (string, error) { return "", io.EOF }

// WaitDecision — один круг: показать форму клиенту, получить решение.
func (u *elicitGateUI) WaitDecision() (gate.Decision, error) {
	schema, message := u.buildRequest()
	base := u.runCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, elicitTimeout)
	defer cancel()
	resp, err := u.srv.sendClientRequest(ctx, "elicitation/create", map[string]interface{}{
		"message":         message,
		"requestedSchema": schema,
	})
	if err != nil {
		// Отмена рана — не «клиент не ответил»: уходить в консоль здесь значит
		// спрашивать человека про ран, который уже останавливают.
		if u.runCtx != nil && u.runCtx.Err() != nil {
			return gate.Decision{}, err
		}
		// Клиент не ответил (обрыв, таймаут, ошибка записи) или отказался
		// принимать запрос. Если есть консоль — уходим в неё, а не хороним ран.
		if d, ok := u.fallbackDecision(err); ok {
			return d, nil
		}
		return gate.Decision{}, err
	}
	if resp.Error != nil {
		if d, ok := u.fallbackDecision(fmt.Errorf("клиент вернул ошибку: %s", resp.Error.Message)); ok {
			return d, nil
		}
		return gate.Decision{}, fmt.Errorf("elicitation: %s", resp.Error.Message)
	}
	var payload struct {
		Action  string                 `json:"action"`
		Content map[string]interface{} `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		if d, ok := u.fallbackDecision(fmt.Errorf("ответ клиента не разобран: %w", err)); ok {
			return d, nil
		}
		return gate.Decision{}, err
	}
	switch strings.ToLower(payload.Action) {
	case "accept":
		edits, skipped := elicitEdits(payload.Content)
		if len(skipped) > 0 {
			// Значения — строки по схеме; если такое значение не разбирается как
			// JSON, гейт его не примет. Не молчим об этом, а отправляем в журнал
			// через stderr клиента: у инструмента нет своего журнала событий.
			u.srv.logf("elicitation: шаг %s, поля %v не разобраны как значения формы и пропущены",
				u.step.ID, skipped)
		}
		return gate.Decision{Action: "accept", Edits: edits, Source: gate.SourceElicitation}, nil
	case "decline":
		// «Нет» человека — это reject, а не отмена рана: семантика гейта
		// (on_reject) остаётся за пайплайном.
		return gate.Decision{Action: "reject", Source: gate.SourceElicitation}, nil
	case "cancel":
		// Человек закрыл диалог, не решая. Отмена гейта = стоп, не accept:
		// та же семантика, что у EOF (v0.23).
		return gate.Decision{}, gate.ErrClosed
	default:
		// Неизвестное действие — не решение: пусть retry-цикл гейта попробует
		// снова, и после пяти попыток ран остановится.
		return gate.Decision{}, fmt.Errorf("elicitation: неизвестное действие %q", payload.Action)
	}
}

// fallbackDecision — переход в консоль после неудачи elicitation.
func (u *elicitGateUI) fallbackDecision(cause error) (gate.Decision, bool) {
	if u.runner == nil || u.runner.fallback == nil {
		return gate.Decision{}, false
	}
	u.runner.mu.Lock()
	if u.runner.used {
		u.runner.mu.Unlock()
		return gate.Decision{}, false
	}
	u.runner.used = true
	fallback := u.runner.fallback
	u.runner.mu.Unlock()

	u.srv.logf("elicitation недоступен для гейта %s (%v) — гейт продолжается в консоли", u.step.ID, cause)
	d, err := fallback.WaitDecision()
	if err != nil {
		return gate.Decision{}, false
	}
	return d, true
}

// buildRequest — сообщение и схема для клиента.
//
// Значения берутся из журнала (событие gate_wait), а не из контекста рана: у
// gate.StructuredUI нет доступа к ctx, а gate_wait пишется ДО WaitDecision и
// содержит ровно ту форму, что увидел бы человек в консоли. Так гейт через
// elicitation показывает те же значения, что и браузерный, не меняя интерфейс
// gate-пакета.
func (u *elicitGateUI) buildRequest() (map[string]interface{}, string) {
	values := u.gateWaitForm()

	properties := map[string]interface{}{}
	required := []string{}
	var lines []string
	for _, f := range u.step.Form {
		value := "<нет данных>"
		if v, ok := values[f.Field]; ok && v != "" {
			value = v
		}
		if f.Editable {
			// Схема elicitation — плоский объект примитивов. Тип поля формы
			// известен не всегда, поэтому просим строку, а значение формы
			// разбираем как JSON при возврате (как это делает терминальный путь).
			desc := "текущее: " + value
			if f.Type != "" {
				desc += " (тип " + f.Type + ")"
			}
			properties[f.Field] = map[string]interface{}{
				"type": "string", "description": desc, "default": value,
			}
			required = append(required, f.Field)
			lines = append(lines, "  * "+f.Field+" = "+value+"   (можно изменить)")
			continue
		}
		lines = append(lines, "  "+f.Field+" = "+value)
	}
	schema := map[string]interface{}{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	message := "Fidespipe · шаг «" + u.step.ID + "» ждёт решения человека.\n"
	if len(lines) > 0 {
		message += "Данные формы:\n" + strings.Join(lines, "\n") + "\n"
	}
	message += "Примите, отклоните или отмените. Правки полей со звёздочкой будут применены."
	return schema, message
}

// gateWaitForm — форма последнего события gate_wait: поле → значение.
func (u *elicitGateUI) gateWaitForm() map[string]string {
	out := map[string]string{}
	dir, err := journal.SafeRunDir(u.srv.runsDir, u.runID)
	if err != nil {
		return out
	}
	res, err := journal.NewReader(dir).EventsBounded(0, runJournalLimits(true))
	if err != nil {
		return out
	}
	for _, e := range res.Events {
		if e["type"] != "gate_wait" {
			continue
		}
		form, ok := e["form"].([]interface{})
		if !ok {
			continue
		}
		for _, raw := range form {
			entry, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			field, _ := entry["field"].(string)
			if field == "" {
				continue
			}
			value, _ := entry["value"].(string)
			out[field] = value
		}
	}
	return out
}

// elicitEdits — значения из content в правки формы.
//
// Клиент по схеме отдаёт строки, а гейт принимает значения того же типа, что и
// поле (validateStructuredEdits сверяет тип с текущим значением или f.Type).
// Поэтому строку разбираем как JSON: "42" → 42, "true" → true, "\"x\"" → "x".
// Не разобралось — значение остаётся строкой: для строкового поля это норма,
// для числового гейт его отбросит и запишет в skipped_edits.
func elicitEdits(content map[string]interface{}) (map[string]interface{}, []string) {
	edits := map[string]interface{}{}
	var skipped []string
	for key, raw := range content {
		switch v := raw.(type) {
		case string:
			var parsed interface{}
			if err := json.Unmarshal([]byte(v), &parsed); err != nil {
				edits[key] = v
				skipped = append(skipped, key)
				continue
			}
			edits[key] = parsed
		default:
			edits[key] = v
		}
	}
	return edits, skipped
}

// elicitGateAvailable — можно ли вести гейт через elicitation.
func (s *Server) elicitGateAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gateElicitation && s.clientElicitation && s.transport != nil
}

// setClientCapabilities — что клиент объявил в initialize.
func (s *Server) setClientCapabilities(params json.RawMessage) {
	if len(params) == 0 {
		return
	}
	var p struct {
		Capabilities struct {
			// Наличие ключа = поддержка: тело может быть пустым объектом.
			Elicitation json.RawMessage `json:"elicitation"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	s.mu.Lock()
	s.clientElicitation = len(p.Capabilities.Elicitation) > 0 && string(p.Capabilities.Elicitation) != "null"
	s.mu.Unlock()
}

// sendClientRequest — запрос ОТ сервера клиенту с ожиданием ответа.
//
// Первый путь этого направления в проекте: до elicitation сервер только
// отвечал. Ответ клиента приходит в stdin обычным сообщением и разбирается по
// признаку «нет method, есть id» (см. parseItem).
func (s *Server) sendClientRequest(ctx context.Context, method string, params interface{}) (*clientResponse, error) {
	s.mu.Lock()
	t := s.transport
	if t == nil {
		s.mu.Unlock()
		return nil, errors.New("нет активной сессии: сервер не обслуживает транспорт")
	}
	s.outboundSeq++
	id := json.RawMessage(strconv.Quote("fidespipe-" + strconv.FormatInt(s.outboundSeq, 10)))
	if s.pending == nil {
		s.pending = map[string]chan *clientResponse{}
	}
	key := requestKey(id)
	ch := make(chan *clientResponse, 1)
	s.pending[key] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}()

	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if err := t.WriteRequest(&Request{JSONRPC: "2.0", ID: id, Method: method, Params: raw}); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// deliverClientResponse — ответ клиента на серверный запрос.
func (s *Server) deliverClientResponse(resp *clientResponse) {
	key := requestKey(resp.ID)
	s.mu.Lock()
	ch := s.pending[key]
	delete(s.pending, key)
	s.mu.Unlock()
	if ch == nil {
		s.logf("ответ клиента на неизвестный запрос %s проигнорирован", key)
		return
	}
	select {
	case ch <- resp:
	default:
	}
}
