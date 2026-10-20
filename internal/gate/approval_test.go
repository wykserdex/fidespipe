package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/runctx"
)

func approvalStep(approval string) *pipeline.Step {
	return &pipeline.Step{
		ID:       "review",
		Plugin:   "core/human_gate",
		Form:     []pipeline.FormField{},
		Actions:  []string{"accept", "reject"},
		Approval: approval,
	}
}

func runGateWithOpts(t *testing.T, st *pipeline.Step, opts GateOptions, ui GateUI) (string, []map[string]interface{}, *journal.Journal, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := journal.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewServiceWithUI(ui)
	res := svc.Run(st, runctx.NewCtx(map[string]interface{}{}), j, opts)
	j.Close()
	raw, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	return res, events, j, dir
}

func TestYesBlockedByHumanApproval(t *testing.T) {
	ui := NewChannelUI()
	st := approvalStep("human")
	done := make(chan string, 1)
	go func() {
		dir := t.TempDir()
		j, _ := journal.NewJournal(dir)
		svc := NewServiceWithUI(ui)
		done <- svc.Run(st, runctx.NewCtx(map[string]interface{}{}), j, GateOptions{Yes: true, Quiet: true, Policy: Policy{AllowAutoApprove: true}})
		j.Close()
	}()
	// если бы auto-accept сработал, результат пришёл бы сразу
	select {
	case res := <-done:
		t.Fatalf("--yes при approval:human не должен авто-принимать, got %q", res)
	case <-time.After(300 * time.Millisecond):
	}
	// человек решает — ран идёт дальше с source=gui
	if !ui.SendDecision(Decision{Action: "accept"}) {
		t.Fatal("SendDecision не принят")
	}
	select {
	case res := <-done:
		if res != "ok" {
			t.Fatalf("после accept got %q", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("гейт не завершился после решения")
	}
}

func TestYesBlockedByPolicy(t *testing.T) {
	ui := NewChannelUI()
	st := approvalStep("any")
	done := make(chan string, 1)
	go func() {
		dir := t.TempDir()
		j, _ := journal.NewJournal(dir)
		svc := NewServiceWithUI(ui)
		done <- svc.Run(st, runctx.NewCtx(map[string]interface{}{}), j, GateOptions{Yes: true, Quiet: true, Policy: Policy{AllowAutoApprove: false}})
		j.Close()
	}()
	select {
	case res := <-done:
		t.Fatalf("--yes при AllowAutoApprove=false не должен авто-принимать, got %q", res)
	case <-time.After(300 * time.Millisecond):
	}
	if !ui.SendDecision(Decision{Action: "accept"}) {
		t.Fatal("SendDecision не принят")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("гейт не завершился")
	}
}

func TestYesAutoHasSource(t *testing.T) {
	st := approvalStep("any")
	res, events, _, _ := runGateWithOpts(t, st, GateOptions{Yes: true, Quiet: true, Policy: Policy{AllowAutoApprove: true}}, NewChannelUI())
	if res != "ok" {
		t.Fatalf("got %q", res)
	}
	for _, ev := range events {
		if ev["type"] == "gate_decision" {
			if ev["source"] != "auto_yes" {
				t.Fatalf("source=%v want auto_yes", ev["source"])
			}
			return
		}
	}
	t.Fatal("нет gate_decision")
}

func TestTerminalHasSource(t *testing.T) {
	st := approvalStep("any")
	res, events, _, _ := runGateWithOpts(t, st, GateOptions{Quiet: true}, &fakeUI{lines: []string{"a"}})
	if res != "ok" {
		t.Fatalf("got %q", res)
	}
	for _, ev := range events {
		if ev["type"] == "gate_decision" && ev["action"] == "accept" {
			if ev["source"] != "terminal" {
				t.Fatalf("source=%v want terminal", ev["source"])
			}
			return
		}
	}
	t.Fatal("нет accept с source=terminal")
}
