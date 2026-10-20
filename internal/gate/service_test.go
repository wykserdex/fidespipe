package gate

// v0.23: гейт больше не одобряет молча — EOF/мусор ≠ accept.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/runctx"
)

// fakeUI — скриптованный ввод: список строк, затем EOF.
type fakeUI struct{ lines []string }

func (f *fakeUI) ReadLine() (string, error) {
	if len(f.lines) == 0 {
		return "", io.EOF
	}
	l := f.lines[0]
	f.lines = f.lines[1:]
	return l, nil
}

func gateStep() *pipeline.Step {
	return &pipeline.Step{
		ID:      "show",
		Plugin:  "core/human_gate",
		Form:    []pipeline.FormField{},
		Actions: []string{"accept", "reject"},
	}
}

// runGate — запуск гейта со своим журналом; возвращает результат + события.
func runGate(t *testing.T, ui GateUI) (string, []map[string]interface{}) {
	t.Helper()
	dir := t.TempDir()
	j, err := journal.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := NewServiceWithUI(ui).Run(gateStep(), runctx.NewCtx(map[string]interface{}{}), j, GateOptions{Quiet: true})
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
			t.Fatalf("битая строка журнала: %q", line)
		}
		events = append(events, ev)
	}
	return res, events
}

func TestGateJunkRetriesThenAccepts(t *testing.T) {
	res, events := runGate(t, &fakeUI{lines: []string{"мусор не из списка", "a"}})
	if res != "ok" {
		t.Fatalf("ожидался accept после переспроса, got %q", res)
	}
	accepted := false
	for _, ev := range events {
		if ev["type"] == "gate_decision" && ev["action"] == "accept" {
			accepted = true
		}
	}
	if !accepted {
		t.Fatalf("нет события accept: %v", events)
	}
}

// EOF (Ctrl+D / закрытый ввод) — стоп рана, НЕ авто-accept.
func TestGateEOFStops(t *testing.T) {
	res, events := runGate(t, &fakeUI{lines: []string{}})
	if res != "abort_item" {
		t.Fatalf("EOF обязан стопнуть ран, got %q", res)
	}
	found := false
	for _, ev := range events {
		if ev["type"] == "gate_decision" && ev["action"] == "stop" {
			found = true
			if r, _ := ev["reason"].(string); !strings.Contains(r, "EOF") {
				t.Fatalf("reason не про EOF: %v", ev)
			}
		}
	}
	if !found {
		t.Fatalf("нет события gate_decision stop/EOF: %v", events)
	}
}

// 5 мусорных попыток — стоп (не бесконечный опрос, не accept).
func TestGateFiveJunkStops(t *testing.T) {
	res, _ := runGate(t, &fakeUI{lines: []string{"x", "y", "z", "w", "q"}})
	if res != "abort_item" {
		t.Fatalf("5 мусорных попыток обязан стопнуть, got %q", res)
	}
}

// reject — как и раньше: on_reject дефолт stop.
func TestGateRejectStops(t *testing.T) {
	res, _ := runGate(t, &fakeUI{lines: []string{"r"}})
	if res != "abort_item" {
		t.Fatalf("reject (on_reject=stop) обязан стопнуть, got %q", res)
	}
}

func TestGateActionsAllowlist(t *testing.T) {
	got := gateActions([]string{"<img src=x onerror=alert(1)>", "ACCEPT", "reject", "accept"})
	if len(got) != 2 || got[0] != "accept" || got[1] != "reject" {
		t.Fatalf("unexpected actions: %v", got)
	}
}
