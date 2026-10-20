package gate

import (
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/runctx"
)

func gateTestStep(t *testing.T) *pipeline.Step {
	t.Helper()
	return &pipeline.Step{
		ID: "review",
		Form: []pipeline.FormField{
			{Field: "steps.check.score", Editable: true, Type: "number"},
			{Field: "steps.check.note", Editable: false},
		},
		Actions:  []string{"accept", "reject"},
		OnReject: "stop",
	}
}

func TestChannelGateAcceptWithEdits(t *testing.T) {
	dir := t.TempDir()
	j, _ := journal.NewJournal(dir)
	defer j.Close()
	ctx := runctx.NewCtx(map[string]interface{}{})
	ctx.SetStep("check", map[string]interface{}{"score": float64(10), "note": "ok"})
	st := gateTestStep(t)
	ui := NewChannelUI()
	done := make(chan string, 1)
	go func() { done <- NewServiceWithUI(ui).Run(st, ctx, j, GateOptions{}) }()
	waitGateWait(t, dir)
	if !ui.SendDecision(Decision{Action: "accept", Edits: map[string]interface{}{"steps.check.score": float64(42)}}) {
		t.Fatal("SendDecision вернул false")
	}
	if got := <-done; got != "ok" {
		t.Fatalf("res = %q, want ok", got)
	}
	dec := lastEventOfType(t, dir, "gate_decision")
	if dec["action"] != "accept" {
		t.Fatalf("action = %v", dec["action"])
	}
	mat, _ := dec["materialized"].(map[string]interface{})
	if mat == nil || mat["score"] != float64(42) {
		t.Fatalf("materialized = %v (ожидаем score=42)", dec["materialized"])
	}
	// правка только editable: read-only поле note не должно попасть в edits
	edits, _ := dec["edits"].(map[string]interface{})
	if edits != nil && len(edits) != 1 {
		t.Fatalf("edits = %v (ожидаем ровно 1 правку)", edits)
	}
}

func TestChannelGateRejectStops(t *testing.T) {
	dir := t.TempDir()
	j, _ := journal.NewJournal(dir)
	defer j.Close()
	ctx := runctx.NewCtx(map[string]interface{}{})
	ctx.SetStep("check", map[string]interface{}{"score": float64(10), "note": "ok"})
	st := gateTestStep(t)
	ui := NewChannelUI()
	done := make(chan string, 1)
	go func() { done <- NewServiceWithUI(ui).Run(st, ctx, j, GateOptions{}) }()
	waitGateWait(t, dir)
	ui.SendDecision(Decision{Action: "reject"})
	if got := <-done; got != "abort_item" {
		t.Fatalf("res = %q, want abort_item (on_reject=stop)", got)
	}
	dec := lastEventOfType(t, dir, "gate_decision")
	if dec["action"] != "reject" {
		t.Fatalf("action = %v", dec["action"])
	}
}

func TestChannelGateEOFStops(t *testing.T) {
	dir := t.TempDir()
	j, _ := journal.NewJournal(dir)
	defer j.Close()
	ctx := runctx.NewCtx(map[string]interface{}{})
	ctx.SetStep("check", map[string]interface{}{"score": float64(10), "note": "ok"})
	st := gateTestStep(t)
	ui := NewChannelUI()
	done := make(chan string, 1)
	go func() { done <- NewServiceWithUI(ui).Run(st, ctx, j, GateOptions{}) }()
	waitGateWait(t, dir)
	if !ui.Close() {
		t.Fatal("Close вернул false")
	}
	if got := <-done; got != "abort_item" {
		t.Fatalf("res = %q, want abort_item (EOF — стоп, v0.23)", got)
	}
	dec := lastEventOfType(t, dir, "gate_decision")
	if dec["action"] != "stop" || !strings.Contains(dec["reason"].(string), "EOF") {
		t.Fatalf("gate_decision = %v", dec)
	}
	// повторный submit после Close — false
	if ui.SendDecision(Decision{Action: "accept"}) {
		t.Fatal("SendDecision после Close должен вернуть false")
	}
}

func TestChannelGateJunkActionsStop(t *testing.T) {
	dir := t.TempDir()
	j, _ := journal.NewJournal(dir)
	defer j.Close()
	ctx := runctx.NewCtx(map[string]interface{}{})
	ctx.SetStep("check", map[string]interface{}{"score": float64(10), "note": "ok"})
	st := gateTestStep(t)
	ui := NewChannelUI()
	done := make(chan string, 1)
	go func() { done <- NewServiceWithUI(ui).Run(st, ctx, j, GateOptions{}) }()
	waitGateWait(t, dir)
	// мусорное решение → gate_retry → сервис ждёт СЛЕДУЮЩЕЕ (retry-цикл):
	// отправляем по одному, синхронизируясь по событию gate_retry
	for i := 1; i <= 5; i++ {
		if !ui.SendDecision(Decision{Action: "bananas"}) {
			t.Fatalf("попытка %d: SendDecision вернул false", i)
		}
		waitEvent(t, dir, "gate_retry", func(e map[string]interface{}) bool {
			return e["attempt"] == float64(i)
		})
	}
	if got := <-done; got != "abort_item" {
		t.Fatalf("res = %q, want abort_item (5 мусорных)", got)
	}
	dec := lastEventOfType(t, dir, "gate_decision")
	if dec["action"] != "stop" {
		t.Fatalf("action = %v", dec["action"])
	}
}

func TestChannelGateBadEditTypeSkipped(t *testing.T) {
	dir := t.TempDir()
	j, _ := journal.NewJournal(dir)
	defer j.Close()
	ctx := runctx.NewCtx(map[string]interface{}{})
	ctx.SetStep("check", map[string]interface{}{"score": float64(10), "note": "ok"})
	st := gateTestStep(t)
	ui := NewChannelUI()
	done := make(chan string, 1)
	go func() { done <- NewServiceWithUI(ui).Run(st, ctx, j, GateOptions{}) }()
	waitGateWait(t, dir)
	// score — number в форме; присылаем строку → правка пропущена, accept стоит
	ui.SendDecision(Decision{Action: "accept", Edits: map[string]interface{}{"steps.check.score": "не число"}})
	if got := <-done; got != "ok" {
		t.Fatalf("res = %q, want ok", got)
	}
	dec := lastEventOfType(t, dir, "gate_decision")
	if dec["action"] != "accept" {
		t.Fatalf("action = %v", dec["action"])
	}
	skipped, _ := dec["skipped_edits"].([]interface{})
	if len(skipped) != 1 || !strings.Contains(skipped[0].(string), "steps.check.score") {
		t.Fatalf("skipped_edits = %v", dec["skipped_edits"])
	}
	mat, _ := dec["materialized"].(map[string]interface{})
	if mat["score"] != float64(10) {
		t.Fatalf("materialized.score = %v, want 10 (правка пропущена → старое значение)", mat)
	}
}

func TestChannelUIRaceFreeSendClose(t *testing.T) {
	// SendDecision и Close одновременно: ровно один выигрывает, WaitDecision
	// всегда возвращает (не вешается) — гонка проверена -race отдельно.
	for i := 0; i < 200; i++ {
		ui := NewChannelUI()
		go ui.Close()
		go ui.SendDecision(Decision{Action: "accept"})
		resCh := make(chan struct {
			d Decision
			e error
		}, 1)
		go func() {
			d, e := ui.WaitDecision()
			resCh <- struct {
				d Decision
				e error
			}{d, e}
		}()
		select {
		case r := <-resCh:
			if r.e == nil && r.d.Action != "accept" {
				t.Fatalf("итер %d: решение без действия: %+v", i, r)
			}
			if r.e != nil && r.e != ErrClosed {
				t.Fatalf("итер %d: неожиданный error %v", i, r.e)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("итер %d: WaitDecision завис", i)
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

func waitGateWait(t *testing.T, dir string) {
	t.Helper()
	waitEvent(t, dir, "gate_wait", nil)
}

// waitEvent — ждать событие типа typ (опционально с условием), 3 c дедлайн.
func waitEvent(t *testing.T, dir string, typ string, match func(map[string]interface{}) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := journal.NewReader(dir).Events()
		if err == nil {
			for _, e := range events {
				if e["type"] == typ && (match == nil || match(e)) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("событие %s не появилось в журнале за 3 c", typ)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func lastEventOfType(t *testing.T, dir string, typ string) map[string]interface{} {
	t.Helper()
	events, err := journal.NewReader(dir).Events()
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	var last map[string]interface{}
	for _, e := range events {
		if e["type"] == typ {
			last = e
		}
	}
	if last == nil {
		t.Fatalf("событие %s не найдено в %s", typ, dir)
	}
	return last
}
