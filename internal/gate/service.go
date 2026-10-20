package gate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/wykserdex/fidespipe/internal/common"
	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/runctx"
)

type GateOptions struct {
	Yes   bool
	Quiet bool
	// v0.9: политика авто-аппрува. Нулевое значение — авто-аппрув
	// запрещён: вызывающий (раннер) обязан явно разрешить его.
	Policy Policy
	// v0.9: RequireHuman — пайплайн требует человека (pipeline.gates:
	// human_only); вычисляет раннер, у гейта нет доступа к pipeline.
	RequireHuman bool
	// AllowAgentAuto — агент может одобрить гейт (source: agent_auto).
	// Структурная граница: approval: human запрещает это независимо от флага.
	AllowAgentAuto bool
}

func gateActions(actions []string) []string {
	out := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, action := range actions {
		normalized := strings.ToLower(strings.TrimSpace(action))
		if (normalized == "accept" || normalized == "reject") && !seen[normalized] {
			seen[normalized] = true
			out = append(out, normalized)
		}
	}
	if len(out) == 0 {
		return []string{"accept", "reject"}
	}
	return out
}

// Policy — кто вправе одобрять гейты без человека.
// AllowAutoApprove=false (MCP-раны, нулевое значение) — --yes игнорируется,
// гейт ждёт решения человека.
type Policy struct {
	AllowAutoApprove bool
}

// Источники решения гейта — поле source события gate_decision.
const (
	SourceTerminal  = "terminal"
	SourceGUI       = "gui"
	SourceAutoYes   = "auto_yes"
	SourceAgentAuto = "agent_auto"
	// SourceElicitation — решение получено через elicitation/create: диалог
	// показал хост MCP-клиента, а не консоль fidespipe и не терминал. Отдельный
	// источник нужен, чтобы в журнале было видно, какой интерфейс решил гейт:
	// «человек в GUI» и «человек в диалоге клиента» — разные каналы доверия.
	SourceElicitation = "mcp_elicitation"
)

// autoApproveAllowed — --yes срабатывает, только если политика разрешает и
// ни шаг (approval: human), ни пайплайн (gates: human_only) не требуют человека.
func autoApproveAllowed(st *pipeline.Step, opts GateOptions) bool {
	return opts.Yes && opts.Policy.AllowAutoApprove && !opts.RequireHuman && st.Approval != "human"
}

// agentAutoAllowed — агент может одобрить гейт только если шаг не требует
// человека явно (approval: human). Это структурная граница: флаг
// AgentCanExec не отменяет гарантию, выставленную человеком руками.
func agentAutoAllowed(st *pipeline.Step, opts GateOptions) bool {
	return opts.AllowAgentAuto && st.Approval != "human"
}

// GateUI — канал ввода human_gate.
// v0.15: шов для GUI/API (M6) — терминал текущая реализация, подставить
// свою можно без изменения Service.
type GateUI interface {
	ReadLine() (string, error)
}

// StdinUI — читает из os.Stdin (дефолт).
// Один bufio.Reader на весь гейт: новый на каждый ReadLine съел бы
// забференный остаток предыдущей строки.
//
// Жизненный цикл: владелец — ран, он создаёт ОДИН StdinUI на прогон и не
// закрывает его. Закрывать нечего и нельзя: os.Stdin принадлежит процессу,
// а не гейту, и закрытие сломало бы следующий гейт и любой другой читатель
// stdin. Владение UI, пришедшим снаружи (ChannelUI, MCP-элицитация), — у
// вызывающего: ран их не создавал и закрывать не должен.
type StdinUI struct {
	reader *bufio.Reader
}

func NewStdinUI() *StdinUI { return &StdinUI{reader: bufio.NewReader(os.Stdin)} }

func (u *StdinUI) ReadLine() (string, error) {
	if u.reader == nil {
		u.reader = bufio.NewReader(os.Stdin)
	}
	return u.reader.ReadString('\n')
}

type Service struct {
	UI GateUI
}

func NewService() *Service { return &Service{UI: NewStdinUI()} }

// NewServiceWithUI — для GUI/API: заменить терминал своим вводом.
func NewServiceWithUI(ui GateUI) *Service {
	if ui == nil {
		return NewService()
	}
	return &Service{UI: ui}
}

// ui — ввод этого сервиса. Ленивая инициализация ОДИН раз на Service, а не
// на каждый Run: иначе Service без явного UI (нулевое значение) на каждый
// гейт завёл бы свой bufio.Reader и второй гейт получил бы EOF — ровно тот
// баг, ради которого комментарий над StdinUI.
func (s *Service) ui() GateUI {
	if s.UI == nil {
		s.UI = NewStdinUI()
	}
	return s.UI
}

// StructuredUI — необязательный интерфейс не-терминального ввода (v0.24):
// один структурированный круг (решение + правки) вместо построчного потока.
// Если GateUI его реализует (ChannelUI — браузер), Service.Run идёт через
// runStructured; терминальный путь остаётся построчным.
type StructuredUI interface {
	WaitDecision() (Decision, error)
}

func kindOf(v interface{}) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	case nil:
		return "null"
	}
	return "unknown"
}

func basename(path string) string {
	parts := strings.Split(path, ".")
	return parts[len(parts)-1]
}

func (s *Service) Materialize(form []pipeline.FormField, ctx *runctx.Ctx, edits map[string]interface{}) map[string]interface{} {
	return gateMaterialize(form, ctx, edits)
}

func gateMaterialize(form []pipeline.FormField, ctx *runctx.Ctx, edits map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	bnCount := map[string]int{}
	for _, f := range form {
		bnCount[basename(f.Field)]++
	}
	for _, f := range form {
		bn := basename(f.Field)
		key := bn
		if bnCount[bn] > 1 {
			parts := strings.Split(f.Field, ".")
			if len(parts) >= 3 && parts[0] == "steps" {
				key = parts[1] + "_" + bn
			} else {
				key = strings.ReplaceAll(f.Field, ".", "_")
			}
		}
		if v, ok := edits[key]; ok {
			out[key] = v
			continue
		}
		if v, ok := edits[bn]; ok && bnCount[bn] == 1 {
			out[key] = v
			continue
		}
		if v, ok := ctx.Get(f.Field); ok {
			out[key] = v
		}
	}
	return out
}

func (s *Service) Run(st *pipeline.Step, ctx *runctx.Ctx, j *journal.Journal, opts GateOptions) string {
	if !opts.Quiet {
		fmt.Printf("\n══ human_gate · %s ══\n", st.ID)
		for _, f := range st.Form {
			mark := " "
			if f.Editable {
				mark = "*"
			}
			if v, ok := ctx.Get(f.Field); ok {
				b, _ := json.Marshal(v)
				fmt.Printf("  %s %s = %s\n", mark, f.Field, common.Truncate(string(b), 500))
			} else {
				fmt.Printf("  %s %s = <нет данных>\n", mark, f.Field)
			}
		}
	}

	if autoApproveAllowed(st, opts) {
		if !opts.Quiet {
			fmt.Println("  [--yes] auto-accept")
		}
		m := gateMaterialize(st.Form, ctx, nil)
		if len(m) > 0 {
			ctx.SetStep(st.ID, m)
		}
		j.Event("gate_decision", map[string]interface{}{"step": st.ID, "action": "accept", "auto": true, "source": SourceAutoYes, "materialized": m})
		return "ok"
	}
	if opts.AllowAgentAuto && agentAutoAllowed(st, opts) {
		m := gateMaterialize(st.Form, ctx, nil)
		if len(m) > 0 {
			ctx.SetStep(st.ID, m)
		}
		j.Event("gate_decision", map[string]interface{}{"step": st.ID, "action": "accept", "auto": true, "source": SourceAgentAuto, "materialized": m})
		return "ok"
	}
	if opts.Yes && !opts.Quiet {
		fmt.Println("  [--yes] не действует: гейт требует решения человека (approval: human / gates: human_only / политика)")
	}

	ui := s.ui()
	// v0.24: не-терминальный ввод (браузер) — структурированный круг,
	// не построчный поток. Терминальный путь ниже не тронут.
	if su, ok := ui.(StructuredUI); ok {
		return s.runStructured(st, ctx, j, su)
	}
	bnCountForEdits := map[string]int{}
	for _, f := range st.Form {
		bnCountForEdits[basename(f.Field)]++
	}
	edits := map[string]interface{}{}
	for _, f := range st.Form {
		if !f.Editable {
			continue
		}
		fmt.Printf("  (*) новое значение для %s (JSON, Enter — оставить): ", f.Field)
		line, rerr := ui.ReadLine()
		if rerr != nil {
			fmt.Println("  ! ввод закрыт (EOF) — перехожу к действию")
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var v interface{}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			fmt.Println("    ! не JSON — правка пропущена")
			continue
		}
		expectedType := f.Type
		if expectedType == "" {
			if srcVal, ok := ctx.Get(f.Field); ok {
				expectedType = kindOf(srcVal)
			}
		}
		if expectedType != "" && kindOf(v) != expectedType {
			fmt.Printf("    ! тип %s не подходит под %s (выведен из %s) — правка пропущена\n", kindOf(v), expectedType, f.Field)
			continue
		}
		edits[editKey(f, bnCountForEdits)] = v
	}

	actions := gateActions(st.Actions)
	keys := make([]string, len(actions))
	for i, a := range actions {
		if a != "" {
			keys[i] = strings.ToLower(a[:1])
		}
	}
	action := ""
	for attempt := 0; attempt < 5; attempt++ {
		fmt.Printf("  действие [%s]: ", strings.Join(keys, "/"))
		ans, rerr := ui.ReadLine()
		if rerr != nil {
			// v0.23: EOF — не «accept». Гейт — это человек; закрытый ввод
			// трактуем как остановку рана, не как молчаливое одобрение.
			fmt.Println("  ! ввод закрыт (EOF) — гейт: стоп")
			j.Event("gate_decision", map[string]interface{}{"step": st.ID, "action": "stop", "reason": "EOF (ввод закрыт)", "source": SourceTerminal})
			return "abort_item"
		}
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans == "" {
			fmt.Println("  ! пусто — введи одно из: " + strings.Join(actions, "/"))
			continue
		}
		matched := false
		for i, a := range actions {
			if ans == a || ans == keys[i] {
				action = a
				matched = true
				break
			}
		}
		if !matched {
			fmt.Printf("  ! %q не из списка — введи: %s\n", ans, strings.Join(actions, "/"))
			continue
		}
		break
	}
	if action == "" {
		// 5 мусорных попыток — тоже не «accept»
		fmt.Println("  ! не удалось распознать действие — гейт: стоп")
		j.Event("gate_decision", map[string]interface{}{"step": st.ID, "action": "stop", "reason": "нераспознанный ввод (5 попыток)", "source": SourceTerminal})
		return "abort_item"
	}

	if action == "reject" {
		j.Event("gate_decision", map[string]interface{}{"step": st.ID, "action": "reject", "source": SourceTerminal})
		if st.OnReject == "" || st.OnReject == "stop" {
			return "abort_item"
		}
		return "ok"
	}
	m := gateMaterialize(st.Form, ctx, edits)
	if len(m) > 0 {
		ctx.SetStep(st.ID, m)
	}
	j.Event("gate_decision", map[string]interface{}{"step": st.ID, "action": "accept", "edits": edits, "materialized": m, "source": SourceTerminal})
	return "ok"
}

// ── v0.24: структурированный гейт (браузер) ──────────────────────────────

// editKey — ключ правки в материализации: имя поля, а при коллизии basename
// (steps.X.field) — «X_field». Общий для терминального и браузерного путей.
func editKey(f pipeline.FormField, bnCount map[string]int) string {
	bn := basename(f.Field)
	if bnCount[bn] > 1 {
		parts := strings.Split(f.Field, ".")
		if len(parts) >= 3 && parts[0] == "steps" {
			return parts[1] + "_" + bn
		}
		return strings.ReplaceAll(f.Field, ".", "_")
	}
	return bn
}

// runStructured — гейт без терминала (v0.24): события в журнал (браузер
// рендерит их), решение — один круг через WaitDecision.
// Семантика v0.23 сохранена: EOF → стоп, нераспознанное действие (5 раз) → стоп.
func (s *Service) runStructured(st *pipeline.Step, ctx *runctx.Ctx, j *journal.Journal, su StructuredUI) string {
	actions := gateActions(st.Actions)
	formView := []map[string]interface{}{}
	for _, f := range st.Form {
		bv := "<нет данных>"
		if v, ok := ctx.Get(f.Field); ok {
			if b, err := json.Marshal(v); err == nil {
				bv = common.Truncate(string(b), 500)
			}
		}
		formView = append(formView, map[string]interface{}{
			"field": f.Field, "editable": f.Editable, "type": f.Type, "value": bv,
		})
	}
	j.Event("gate_wait", map[string]interface{}{
		"step": st.ID, "form": formView, "actions": actions,
	})

	var action string
	var edits map[string]interface{}
	var last Decision
	for attempt := 0; attempt < 5; attempt++ {
		d, err := su.WaitDecision()
		if err != nil {
			// EOF (вкладку закрыли, ран убивают) — стоп, не accept (v0.23).
			j.Event("gate_decision", map[string]interface{}{
				"step": st.ID, "action": "stop", "reason": "ввод закрыт (EOF)", "source": SourceGUI,
			})
			return "abort_item"
		}
		last = d
		matched := false
		for _, a := range actions {
			if strings.EqualFold(d.Action, a) {
				action, matched = a, true
				break
			}
		}
		if !matched {
			j.Event("gate_retry", map[string]interface{}{
				"step": st.ID, "attempt": attempt + 1,
				"reason": fmt.Sprintf("действие %q не из списка %v", d.Action, actions),
			})
			continue
		}
		edits = d.Edits
		break
	}
	if action == "" {
		j.Event("gate_decision", decisionKV(last, map[string]interface{}{
			"step": st.ID, "action": "stop", "reason": "нераспознанное действие (5 попыток)",
		}))
		return "abort_item"
	}

	if action == "reject" {
		j.Event("gate_decision", decisionKV(last, map[string]interface{}{"step": st.ID, "action": "reject"}))
		if st.OnReject == "" || st.OnReject == "stop" {
			return "abort_item"
		}
		return "ok"
	}

	clean, skipped := validateStructuredEdits(st, ctx, edits)
	m := gateMaterialize(st.Form, ctx, clean)
	if len(m) > 0 {
		ctx.SetStep(st.ID, m)
	}
	kv := decisionKV(last, map[string]interface{}{"step": st.ID, "action": "accept", "edits": clean, "materialized": m})
	if len(skipped) > 0 {
		kv["skipped_edits"] = skipped
	}
	j.Event("gate_decision", kv)
	return "ok"
}

// validateStructuredEdits — правки из браузера: ключ — полный путь поля
// (как в gate_wait), валидация типа как в терминальном пути (f.Type или
// тип текущего значения). Неподходящие — пропускаются, список в skipped.
func validateStructuredEdits(st *pipeline.Step, ctx *runctx.Ctx, edits map[string]interface{}) (map[string]interface{}, []string) {
	clean := map[string]interface{}{}
	var skipped []string
	if len(edits) == 0 {
		return clean, skipped
	}
	bnCount := map[string]int{}
	for _, f := range st.Form {
		bnCount[basename(f.Field)]++
	}
	for _, f := range st.Form {
		if !f.Editable {
			continue
		}
		v, ok := edits[f.Field]
		if !ok {
			continue
		}
		expectedType := f.Type
		if expectedType == "" {
			if srcVal, ok := ctx.Get(f.Field); ok {
				expectedType = kindOf(srcVal)
			}
		}
		if expectedType != "" && kindOf(v) != expectedType {
			skipped = append(skipped, fmt.Sprintf("%s (типа %s, а ожидается %s)", f.Field, kindOf(v), expectedType))
			continue
		}
		clean[editKey(f, bnCount)] = v
	}
	return clean, skipped
}

// decisionKV — source (+ session-хэш, если есть) структурированного решения.
// Секрет сессии сюда не попадает никогда: Decision.Session — уже хэш.
func decisionKV(d Decision, kv map[string]interface{}) map[string]interface{} {
	src := d.Source
	if src == "" {
		src = SourceGUI
	}
	kv["source"] = src
	if d.Session != "" {
		kv["session"] = d.Session
	}
	return kv
}
