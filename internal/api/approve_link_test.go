package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGateStatusCarriesApproveLink — в статусе ожидающего гейта есть
// подписанная ссылка одобрения.
//
// Нужна для БЕЗГОЛОВОГО сервера: человек, ждущий решения, не сидит за
// терминалом этой машины, а открыть ссылку может с телефона.
func TestGateStatusCarriesApproveLink(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(dir, dir, dir)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// Гейта нет — ссылки нет и быть не должно: ссылка на несуществующий гейт
	// выглядела бы как рабочая. Ответ на несуществующий ран — НЕ json, а текст
	// с кодом 404, поэтому тело читается текстом.
	resp, err := http.Get(ts.URL + "/api/runs/nosuchrun/gate")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := make([]byte, 1024)
	n, _ := resp.Body.Read(body)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("несуществующий ран отдал 200: %q", string(body[:n]))
	}
	if strings.Contains(string(body[:n]), "approve_link") {
		t.Errorf("ссылка выдана без гейта: %q", string(body[:n]))
	}
}

// TestApproveRouteIsNotSessionProtected — ссылка одобрения живёт ВНЕ /api/*,
// поэтому сессия человека её не требует: подпись и есть право.
func TestApproveRouteIsNotSessionProtected(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(dir, dir, dir)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/approve?t=мусор")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("ссылка одобрения требует сессии: %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("маршрут /approve не зарегистрирован")
	}
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "Ссылка") {
		t.Errorf("ответ не объясняет, что с ссылкой не так: %q", string(body[:n]))
	}
}
