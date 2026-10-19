package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/wedra/internal/gate"
	"github.com/wykserdex/wedra/internal/journal"
)

// Ран с ожидающим гейтом, ссылка одобрения и её открытие — целиком.
func TestE2EApproveLink(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(dir, dir, dir)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// Готовим журнал рана с ожидающим гейтом.
	runDir := dir + "/run1"
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := journal.NewJournal(runDir)
	if err != nil {
		t.Fatal(err)
	}
	j.Event("run_start", map[string]interface{}{"pipeline": "p", "pipeline_hash": "h"})
	j.Event("gate_wait", map[string]interface{}{
		"step":    "review",
		"form":    []map[string]interface{}{{"field": "comment", "editable": true, "type": "string", "value": "<нет>"}},
		"actions": []string{"accept", "reject"},
	})
	j.Close()

	// Регистрируем живой гейт у сервера.
	ui := gate.NewChannelUI()
	srv.AttachRun("run1", ui, nil)

	resp, err := http.Get(ts.URL + "/api/runs/run1/gate")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["pending"] != true {
		t.Fatalf("гейт не в ожидании: %v", out)
	}
	link, _ := out["approve_link"].(string)
	if link == "" {
		t.Fatalf("ссылка не выдана: %v", out)
	}
	t.Logf("ссылка: %s", link[:60]+"…")

	// Открываем ссылку — форма.
	resp2, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 2048)
	n, _ := resp2.Body.Read(body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("страница ссылки: %d %q", resp2.StatusCode, string(body[:n]))
	}
	if !strings.Contains(string(body[:n]), "comment") {
		t.Errorf("в форме нет поля гейта: %q", string(body[:n]))
	}
	t.Logf("форма: %s", string(body[:min(220, n)]))

	// Решение читаем через WaitDecision: канал внутри приватный, а этот метод
	// публичный и ровно тем же пользуется гейт.
	got := make(chan gate.Decision, 1)
	go func() {
		d, err := ui.WaitDecision()
		if err == nil {
			got <- d
		}
	}()

	// Одобряем.
	resp3, err := http.PostForm(link, map[string][]string{"action": {"accept"}, "comment": {"ок"}})
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("одобрение: %d", resp3.StatusCode)
	}
	select {
	case d := <-got:
		if d.Action != "accept" {
			t.Errorf("действие = %q, ожидалось accept", d.Action)
		}
		if d.Edits["comment"] != "ок" {
			t.Errorf("правки = %v, ожидался comment=ок", d.Edits)
		}
		t.Logf("решение доставлено: %+v", d)
	case <-time.After(3 * time.Second):
		t.Fatal("решение не дошло до гейта")
	}
}
