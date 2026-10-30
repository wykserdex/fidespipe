package bot

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in     string
		kind   string
		target string
		ok     bool
	}{
		{"@wykserdex", "username", "wykserdex", true},
		{"wykserdex", "username", "wykserdex", true},
		{"  @Some_User123  ", "username", "Some_User123", true},
		{"test@yandex.ru", "email", "test@yandex.ru", true},
		{"a@b.co", "email", "a@b.co", true},
		{"@ab", "", "", false},       // короче 3 символов
		{"@bad-name", "", "", false}, // дефиса нет в нике
		{"-input=x", "", "", false},  // не должно уехать флагом в CLI
		{"--input=x", "", "", false},
		{"@телеграм", "", "", false}, // только латиница
		{"a b@c.ru", "", "", false},  // пробел внутри адреса
		{"plain", "", "", false},     // 5 букв — валидный ник? да, но проверим ниже
	}
	for _, c := range cases {
		kind, target, err := parseTarget(c.in)
		if c.in == "plain" {
			// «plain» — обычный ник без @: должен разобраться как username.
			if err != nil || kind != "username" || target != "plain" {
				t.Errorf("parseTarget(%q) = %q %q %v, хочу username plain", c.in, kind, target, err)
			}
			continue
		}
		if c.ok && (err != nil || kind != c.kind || target != c.target) {
			t.Errorf("parseTarget(%q) = %q %q %v, хочу %s %s", c.in, kind, target, err, c.kind, c.target)
		}
		if !c.ok && err == nil {
			t.Errorf("parseTarget(%q) не должна разбираться, получили %q %q", c.in, kind, target)
		}
	}
}

func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	o := &Order{ID: "abc12345", UserID: 42, ChatID: 42, Kind: "username", Target: "wykserdex", Status: StatusScanned, CreatedAt: time.Now().UTC()}
	if err := s.Save(o); err != nil {
		t.Fatal(err)
	}
	o.Status = StatusAwaitingPayment
	o.InvoiceID = 777
	if err := s.Save(o); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get("abc12345")
	if !ok {
		t.Fatal("заказ потерялся после перечитывания хранилища")
	}
	if got.Status != StatusAwaitingPayment || got.InvoiceID != 777 {
		t.Fatalf("последнее состояние не перекрыло предыдущее: %+v", got)
	}
	if awaiting := s2.AwaitingPayment(); len(awaiting) != 1 {
		t.Fatalf("AwaitingPayment = %d, хочу 1", len(awaiting))
	}
}

func TestReadDossierFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := map[string]any{
		"input": map[string]any{"username": "wykserdex"},
		"steps": map[string]any{
			"sherlock": map[string]any{
				"checked": 19,
				"claimed": 2,
				"found": []any{
					map[string]any{"site": "GitHub", "url": "https://github.com/wykserdex"},
					map[string]any{"site": "VK", "url": "https://vk.com/wykserdex"},
				},
			},
		},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "context.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Журнал — источник списка шагов: по context.json проваленный шаг
	// неотличим от того, которого в пайплайне не было.
	journalEvents := strings.Join([]string{
		`{"type":"step_start","step":"sherlock","attempt":1}`,
		`{"type":"step_end","step":"sherlock","status":"ok","exit_code":0,"error":null}`,
		`{"type":"step_start","step":"holehe","attempt":1}`,
		`{"type":"step_end","step":"holehe","status":"domain_error","exit_code":1,` +
			`"error":{"code":"no_report","message":"holehe не дал CSV"}}`,
		`{"type":"step_skipped","step":"holehe","reason":"on_error"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(journalEvents), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := ReadDossier(dir, "username", "wykserdex")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Profiles) != 2 || d.Profiles[0].Site != "GitHub" {
		t.Fatalf("профили не собрались: %+v", d.Profiles)
	}
	// holehe в снапшоте нет, но шаг заявлен и упал: источник обязан быть
	// помечен неотработавшим с причиной, а не «ноль находок» — это разные
	// заявления покупателю.
	var holehe SourceStat
	found := false
	for _, s := range d.Sources {
		if s.ID == "holehe" {
			holehe, found = s, true
		}
	}
	if !found {
		t.Fatal("заявленный шаг holehe пропал из списка источников")
	}
	if holehe.OK {
		t.Fatal("отсутствующий holehe помечен отработавшим")
	}
	if holehe.Reason == "" {
		t.Fatal("у проваленного источника нет причины в отчёте")
	}
	var sherlock SourceStat
	for _, s := range d.Sources {
		if s.ID == "sherlock" {
			sherlock = s
		}
	}
	if !sherlock.OK || sherlock.Checked != 19 {
		t.Fatalf("sherlock статус: %+v", sherlock)
	}

	out := t.TempDir()
	files, err := WriteReport(d, dir, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("файлов %d, хочу 3", len(files))
	}
	html, err := os.ReadFile(filepath.Join(out, "report.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"wykserdex", "https://github.com/wykserdex", "не отработал", "holehe не дал CSV"} {
		if !strings.Contains(string(html), want) {
			t.Errorf("в report.html нет %q", want)
		}
	}
}

func TestPathWithAddsScriptsDir(t *testing.T) {
	got := pathWith("/py/Scripts", []string{"HOME=/h", "PATH=/usr/bin:/bin"})
	if len(got) != 2 {
		t.Fatalf("неожиданное число переменных: %v", got)
	}
	if got[1] != "PATH=/py/Scripts"+string(os.PathListSeparator)+"/usr/bin:/bin" {
		t.Fatalf("PATH не расширен: %q", got[1])
	}
	// Без PATH в окружении переменную надо создать, а не потерять каталог.
	got = pathWith("/py/Scripts", []string{"HOME=/h"})
	if got[1] != "PATH=/py/Scripts" {
		t.Fatalf("PATH не создан: %v", got)
	}
	if p := pathWith("", []string{"PATH=/usr/bin"}); p[0] != "PATH=/usr/bin" {
		t.Fatalf("пустой extraPATH трогает окружение: %v", p)
	}
}

// ScriptsDir обязана найти каталог console-скриптов того Python, который
// ставил доноров: без этого holehe в платном отчёте не отработает.
func TestScriptsDirFindsRealInterpreter(t *testing.T) {
	if _, err := exec.LookPath("python"); err != nil {
		t.Skip("python недоступен в тестах")
	}
	dir := ScriptsDir("python")
	if dir == "" {
		t.Fatal("ScriptsDir вернула пусто для python из PATH")
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("ScriptsDir указала не на каталог: %s (%v)", dir, err)
	}
	if got := ScriptsDir("python-которого-нет"); got != "" {
		t.Fatalf("для отсутствующего интерпретатора должен быть пусто, получили %q", got)
	}
}

// Два источника по одному нику: находки maigret должны попасть в список
// профилей без дублей, а его «checked» — не раздуть счётчик проверок.
func TestReadDossierMergesSecondSource(t *testing.T) {
	dir := t.TempDir()
	snapshot := map[string]any{
		"steps": map[string]any{
			"sherlock": map[string]any{
				"checked": 482,
				"found": []any{
					map[string]any{"site": "GitHub", "url": "https://github.com/wykserdex"},
					map[string]any{"site": "VK", "url": "https://vk.com/wykserdex"},
				},
			},
			"maigret": map[string]any{
				// у maigret simple-отчёт = только находки: checked = 2 здесь
				// это две записи, а не два проверенных сайта
				"checked": 3,
				"found": []any{
					map[string]any{"site": "GitHub", "url": "https://www.github.com/wykserdex/"},
					map[string]any{"site": "FilmWeb", "url": "https://filmweb.pl/user/wykserdex"},
					map[string]any{"site": "Telegram", "url": "https://t.me/wykserdex"},
				},
			},
		},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "context.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	events := strings.Join([]string{
		`{"type":"step_start","step":"sherlock"}`,
		`{"type":"step_end","step":"sherlock","status":"ok"}`,
		`{"type":"step_start","step":"maigret"}`,
		`{"type":"step_end","step":"maigret","status":"ok"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := ReadDossier(dir, "username", "wykserdex")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Profiles) != 4 {
		t.Fatalf("профилей %d, хочу 4 без дубля GitHub: %+v", len(d.Profiles), d.Profiles)
	}
	seen := map[string]int{}
	for _, p := range d.Profiles {
		seen[p.URL]++
	}
	if seen["https://github.com/wykserdex"] != 1 {
		t.Fatalf("GitHub продублировался: %+v", d.Profiles)
	}
	if d.Checked != 482 {
		t.Fatalf("проверок %d, хочу 482: checked maigret — это число находок, не объём проверки", d.Checked)
	}
	var maigret SourceStat
	for _, s := range d.Sources {
		if s.ID == "maigret" {
			maigret = s
		}
	}
	if !maigret.OK || maigret.Found != 3 || maigret.Checked != 0 {
		t.Fatalf("maigret в списке источников: %+v", maigret)
	}
}

// TestRunnerFindsDonorOffPath — платный путь целиком: бин донора (holehe.exe)
// лежит в Scripts интерпретатора, которого нет в PATH, и Runner обязан передать
// расширенный PATH движку так, чтобы плагин его увидел. Без этого платный
// отчёт по email теряет главный источник и продаёт меньше, чем обещает.
//
// Тест требует собранных доноров и сети, поэтому по умолчанию пропускается:
// FIDESPIPE_BOT_E2E=1 go test ./internal/bot -run Donor -timeout 20m
func TestRunnerFindsDonorOffPath(t *testing.T) {
	if os.Getenv("FIDESPIPE_BOT_E2E") == "" {
		t.Skip("нужны доноры и сеть: FIDESPIPE_BOT_E2E=1")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	scripts := ScriptsDir("python")
	if scripts == "" {
		t.Fatal("ScriptsDir не нашла каталог скриптов")
	}
	target := "vyachesclavkarpov@gmail.com"

	// Шаг holehe идёт с on_error: skip, поэтому ран не падает: разницу видно
	// только в самом отчёте. Проверяем ровно то, что покупателю продаётся.
	os.Setenv("PATH", withoutDir(os.Getenv("PATH"), scripts))
	r := NewRunner(filepath.Join(root, "fidespipe.exe"), root, t.TempDir(), "", 1)
	dir, err := r.Run(context.Background(), "nopath", "email_full.yaml", "target", target)
	if err != nil {
		t.Fatalf("ран без Scripts упал совсем: %v", err)
	}
	if holeheOK(t, dir, target) {
		t.Fatal("holehe отработал без Scripts в PATH — тест ничего не доказывает")
	}

	r2 := NewRunner(filepath.Join(root, "fidespipe.exe"), root, t.TempDir(), scripts, 1)
	dir, err = r2.Run(context.Background(), "withpath", "email_full.yaml", "target", target)
	if err != nil {
		t.Fatalf("с расширенным PATH ран упал: %v", err)
	}
	if !holeheOK(t, dir, target) {
		t.Fatalf("Scripts в PATH передан, но holehe не отработал: %s", dir)
	}
}

// holeheOK — отработал ли источник в прогоне из этого каталога.
func holeheOK(t *testing.T, runDir, target string) bool {
	t.Helper()
	d, err := ReadDossier(runDir, "email", target)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range d.Sources {
		if s.ID == "holehe" {
			t.Logf("holehe: ok=%v checked=%d leaks=%d reason=%q", s.OK, s.Checked, s.Found, s.Reason)
			return s.OK
		}
	}
	t.Fatalf("в отчёте нет источника holehe: %+v", d.Sources)
	return false
}

// withoutDir — вычеркнуть каталог из PATH (чтобы проверить «до» и «после»).
func withoutDir(pathList, dir string) string {
	want := filepath.Clean(dir)
	var keep []string
	for _, p := range filepath.SplitList(pathList) {
		if p == "" || strings.EqualFold(filepath.Clean(p), want) {
			continue
		}
		keep = append(keep, p)
	}
	return strings.Join(keep, string(os.PathListSeparator))
}

// Быстрый скан состоит из одного шага: источник, которого в пайплайне не было,
// не должен появляться в тексте как «не отработал» — покупатель прочитает это
// как поломку платной части, а бесплатной строки про holehe там нет вовсе.
func TestQuickSummarySilentAboutAbsentSources(t *testing.T) {
	dir := t.TempDir()
	raw, err := json.Marshal(map[string]any{
		"steps": map[string]any{
			"sherlock": map[string]any{"checked": 19, "found": []any{
				map[string]any{"site": "GitHub", "url": "https://github.com/wykserdex"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "context.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	events := "{\"type\":\"step_start\",\"step\":\"sherlock\"}\n" +
		"{\"type\":\"step_end\",\"step\":\"sherlock\",\"status\":\"ok\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := ReadDossier(dir, "username", "wykserdex")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Sources) != 1 || d.Sources[0].ID != "sherlock" {
		t.Fatalf("источников %v, хочу один sherlock", d.Sources)
	}
	text := d.QuickSummary()
	if strings.Contains(text, "holehe") || strings.Contains(text, "не отработал") {
		t.Fatalf("в бесплатном тексте лишнее: %q", text)
	}
	if !strings.Contains(text, "19") || !strings.Contains(text, "1") {
		t.Fatalf("в бесплатном тексте нет счётчиков: %q", text)
	}
}
