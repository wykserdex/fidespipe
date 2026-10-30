package bot

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wykserdex/fidespipe/internal/journal"
)

// Dossier — содержимое отчёта, собранное из context.json конкретного прогона.
// Ничего не вычисляется «примерно»: если источник упал, он помечен упавшим,
// а если находок нет, в отчёте написано, что находок нет.
type Dossier struct {
	Kind        string
	Target      string
	RunID       string
	Generated   time.Time
	Profiles    []Profile
	Leaks       []Leak
	Checked     int
	RateLimited int
	Email       *EmailInfo
	Sources     []SourceStat
	// HasLeakSource — в прогоне был шаг, умеющий давать «записи об
	// использовании» (holehe). Нужен, чтобы не рисовать пустой раздел там,
	// где такой источник не заявлялся: пустой раздел читают как «проверили и
	// не нашли».
	HasLeakSource bool
}

type Profile struct {
	Site string
	URL  string
}

type Leak struct {
	Site   string
	Domain string
}

type EmailInfo struct {
	MX            []string
	HasSyntax     bool
	SyntaxValid   bool
	HasDisposable bool
	IsDisposable  bool
}

// checkedIsAttempts — у каких источников поле checked означает «сколько сайтов
// проверили». У maigret simple-отчёт содержит только найденные профили, и его
// checked = число находок: складывать его с sherlock значит соврать про объём
// проверки. Сам плагин править нельзя — его хэш стоит в allow-list доверия,
// а изменённый файл ядро уже не пустит без конфига оператора.
var checkedIsAttempts = map[string]bool{
	"sherlock": true,
	"holehe":   true,
}

type SourceStat struct {
	ID      string
	Title   string
	OK      bool
	Checked int
	Found   int
	Reason  string
}

// sourceTitles — как объяснить источник человеку. Шаг может назваться как
// угодно (id выбирает автор пайплайна), поэтому неизвестный id показывается
// как есть: выдумывать ему описание хуже, чем показать имя шага.
var sourceTitles = map[string]string{
	"sherlock":   "Sherlock — профили по сайтам",
	"maigret":    "Maigret — профили по сайтам (вторая база)",
	"holehe":     "Holehe — где используется адрес",
	"syntax":     "Синтаксис адреса и MX домена",
	"disposable": "Проверка одноразового домена",
}

func ReadDossier(runDir, kind, target string) (*Dossier, error) {
	snapshot, err := journal.NewReader(runDir).ContextSnapshot()
	if err != nil {
		return nil, fmt.Errorf("context.json: %w", err)
	}
	events, err := journal.NewReader(runDir).Events()
	if err != nil {
		return nil, fmt.Errorf("journal.jsonl: %w", err)
	}
	d := &Dossier{
		Kind:      kind,
		Target:    target,
		RunID:     filepath.Base(filepath.Clean(runDir)),
		Generated: time.Now().UTC(),
	}
	steps, _ := snapshot["steps"].(map[string]any)

	// Источники берём из журнала, а не из context.json: context.json хранит
	// только выходы дошедших шагов, и по нему проваленный шаг неотличим от
	// шага, которого в пайплайне не было. Для платного отчёта это важно:
	// «проверено 0 сайтов» и «источник не ответил» — разные результаты.
	for _, step := range declaredSteps(events) {
		s := SourceStat{ID: step.ID, Title: sourceTitles[step.ID], OK: step.Status == "ok", Reason: truncateReason(step.Reason)}
		if s.Title == "" {
			s.Title = step.ID
		}
		if out, ok := stepOut(steps, step.ID); ok {
			if checkedIsAttempts[step.ID] {
				s.Checked = intOf(out, "checked")
				d.Checked += s.Checked
			}
			s.Found = len(arrayOf(out, "found")) + len(arrayOf(out, "leaks"))
		}
		if step.ID == "holehe" {
			d.HasLeakSource = true
		}
		d.Sources = append(d.Sources, s)
	}

	if out, ok := stepOut(steps, "sherlock"); ok {
		d.Profiles = append(d.Profiles, profilesOf(out)...)
	}
	if out, ok := stepOut(steps, "maigret"); ok {
		d.Profiles = append(d.Profiles, profilesOf(out)...)
	}
	d.Profiles = dedupProfiles(d.Profiles)
	if out, ok := stepOut(steps, "holehe"); ok {
		d.RateLimited = intOf(out, "rate_limited")
		for _, item := range arrayOf(out, "leaks") {
			m, _ := item.(map[string]any)
			d.Leaks = append(d.Leaks, Leak{Site: stringOf(m, "site"), Domain: stringOf(m, "domain")})
		}
	}

	if kind == "email" {
		info := &EmailInfo{}
		if out, ok := stepOut(steps, "syntax"); ok {
			if v, ok := out["syntax"].(bool); ok {
				info.HasSyntax = true
				info.SyntaxValid = v
			}
			for _, item := range arrayOf(out, "mx") {
				if s, ok := item.(string); ok {
					info.MX = append(info.MX, s)
				}
			}
		}
		if out, ok := stepOut(steps, "disposable"); ok {
			if v, ok := out["disposable"].(bool); ok {
				info.HasDisposable = true
				info.IsDisposable = v
			}
		}
		d.Email = info
	}
	return d, nil
}

type stepRecord struct {
	ID     string
	Status string
	Reason string
}

// declaredSteps — шаги в порядке объявления, с итоговым статусом и причиной.
// Шаг без step_end (таймаут, отмена, падение процесса) остаётся с пустым
// статусом — в отчёте это «не отработал», что честно: результата нет.
func declaredSteps(events []map[string]any) []stepRecord {
	order := []string{}
	status := map[string]string{}
	reason := map[string]string{}
	for _, e := range events {
		id, _ := e["step"].(string)
		if id == "" {
			continue
		}
		switch e["type"] {
		case "step_start":
			if _, dup := status[id]; !dup {
				order = append(order, id)
				status[id] = ""
			}
		case "step_end":
			if _, ok := status[id]; !ok {
				order = append(order, id)
			}
			status[id], _ = e["status"].(string)
			if errObj, ok := e["error"].(map[string]any); ok {
				if msg, _ := errObj["message"].(string); msg != "" {
					reason[id] = msg
				}
			}
		}
	}
	out := make([]stepRecord, 0, len(order))
	for _, id := range order {
		out = append(out, stepRecord{ID: id, Status: status[id], Reason: reason[id]})
	}
	return out
}

func profilesOf(out map[string]any) []Profile {
	var list []Profile
	for _, item := range arrayOf(out, "found") {
		m, _ := item.(map[string]any)
		list = append(list, Profile{Site: stringOf(m, "site"), URL: stringOf(m, "url")})
	}
	return list
}

// dedupProfiles — два источника часто дают один и тот же профиль (ник на одном
// и том же сайте). Покупателю список с дублями выглядит как надутый результат.
// Ключ — адрес без схемы, без www. и без слэша: sherlock отдаёт
// https://www.github.com/x, maigret — https://github.com/x, и это одна находка.
func dedupProfiles(in []Profile) []Profile {
	seen := map[string]bool{}
	out := make([]Profile, 0, len(in))
	for _, p := range in {
		key := profileKey(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

func profileKey(p Profile) string {
	u := p.URL
	if u == "" {
		return strings.ToLower(p.Site)
	}
	scheme, rest, hadScheme := strings.Cut(u, "://")
	if !hadScheme || (strings.ToLower(scheme) != "http" && strings.ToLower(scheme) != "https") {
		rest = u
	}
	// Хост приводим к нижнему регистру и снимаем www.: это не разные профили.
	// Регистр пути не трогаем — на части площадок ник чувствителен к регистру,
	// и склеить два разных профиля хуже, чем показать два одинаковых.
	host, path, hasPath := strings.Cut(rest, "/")
	host = strings.ToLower(strings.TrimPrefix(host, "www."))
	if !hasPath {
		return host
	}
	return host + "/" + strings.TrimRight(path, "/")
}

// QuickSummary — текст бесплатного скана в чат. Содержит только счётчики:
// ссылки и детали — это платная часть, бесплатно их не отдаём.
func (d *Dossier) QuickSummary() string {
	var b strings.Builder
	if d.Kind == "username" {
		fmt.Fprintf(&b, "Быстрый скан @%s:\n", d.Target)
		if len(d.Profiles) > 0 {
			fmt.Fprintf(&b, "найдено профилей: %d (проверено сайтов: %d).\n", len(d.Profiles), d.Checked)
		} else {
			fmt.Fprintf(&b, "профилей не найдено (проверено сайтов: %d).\n", d.Checked)
		}
	} else {
		fmt.Fprintf(&b, "Проверка %s:\n", d.Target)
		if d.Email != nil {
			if d.Email.HasSyntax {
				if d.Email.SyntaxValid {
					b.WriteString("синтаксис: верный.\n")
				} else {
					b.WriteString("синтаксис: НЕверный.\n")
				}
			}
			fmt.Fprintf(&b, "MX-записей домена: %d.\n", len(d.Email.MX))
			if d.Email.HasDisposable && d.Email.IsDisposable {
				b.WriteString("домен одноразовый (временная почта).\n")
			} else if d.Email.HasDisposable {
				b.WriteString("домен не одноразовый.\n")
			}
		}
	}
	for _, s := range d.Sources {
		if !s.OK {
			if s.Reason != "" {
				fmt.Fprintf(&b, "источник %s не отработал: %s\n", s.ID, truncateReason(s.Reason))
			} else {
				fmt.Fprintf(&b, "источник %s не отработал.\n", s.ID)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// truncateReason — причина в чате не должна разрастаться до лога.
func truncateReason(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) <= 160 {
		return s
	}
	return string([]rune(s)[:160]) + "…"
}

func (d *Dossier) FullSummary() string {
	var b strings.Builder
	worked, total := 0, len(d.Sources)
	for _, s := range d.Sources {
		if s.OK {
			worked++
		}
	}
	fmt.Fprintf(&b, "Досье готово: %s\n", d.Target)
	if d.Kind == "username" {
		fmt.Fprintf(&b, "профилей: %d · проверок по сайтам: %d\n", len(d.Profiles), d.Checked)
	} else {
		fmt.Fprintf(&b, "сайтов, где используется адрес: %d · сайтов без ответа: %d\n", len(d.Leaks), d.RateLimited)
	}
	if worked < total {
		fmt.Fprintf(&b, "источников отработало: %d из %d — не отработавшие перечислены в отчёте\n", worked, total)
	}
	b.WriteString("Внутри: report.html (отчёт), evidence.json (сырые данные), journal.jsonl (журнал прогона).")
	return b.String()
}

// WriteReport пишет три файла: человекочитаемый HTML, сырой context.json
// (evidence.json) и журнал прогона. Журнал — это доказательство, что строки
// отчёта получены движком, а не нарисованы: run_id совпадает в обоих файлах.
func WriteReport(d *Dossier, runDir, outDir string) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, err
	}
	htmlPath := filepath.Join(outDir, "report.html")
	f, err := os.OpenFile(htmlPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := reportTemplate.Execute(f, d); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	snapshot, err := journal.NewReader(runDir).ContextSnapshot()
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return nil, err
	}
	evidencePath := filepath.Join(outDir, "evidence.json")
	if err := os.WriteFile(evidencePath, raw, 0o600); err != nil {
		return nil, err
	}

	journalPath := filepath.Join(outDir, "journal.jsonl")
	if err := copyFile(journalPath, filepath.Join(runDir, "journal.jsonl")); err != nil {
		return nil, err
	}
	return []string{htmlPath, evidencePath, journalPath}, nil
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func stepOut(steps map[string]any, id string) (map[string]any, bool) {
	if steps == nil {
		return nil, false
	}
	m, ok := steps[id].(map[string]any)
	return m, ok && m != nil
}

func stringOf(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func intOf(m map[string]any, key string) int {
	// JSON числа приезжают float64; дробной части у счётчиков нет.
	f, _ := m[key].(float64)
	return int(f)
}

func arrayOf(m map[string]any, key string) []any {
	a, _ := m[key].([]any)
	return a
}

var reportTemplate = template.Must(template.New("dossier").Parse(`<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Досье: {{.Target}}</title>
<style>
  body{background:#0e1116;color:#dfe6ee;font:15px/1.6 ui-sans-serif,system-ui,sans-serif;margin:0;padding:32px}
  main{max-width:860px;margin:0 auto}
  h1{font-size:24px;margin:0 0 4px}
  h2{font-size:17px;margin:32px 0 10px;color:#7ee0c3}
  .meta{color:#8b96a5;font-size:13px}
  table{border-collapse:collapse;width:100%;margin-top:8px}
  td,th{padding:7px 10px;border-bottom:1px solid #232a35;text-align:left;font-size:14px}
  th{color:#8b96a5;font-weight:600;font-size:12px;text-transform:uppercase;letter-spacing:.06em}
  a{color:#7ee0c3;text-decoration:none}
  .num{font-size:40px;font-weight:700;color:#7ee0c3}
  .cards{display:flex;gap:24px;margin-top:16px}
  .card{background:#151a22;border:1px solid #232a35;border-radius:10px;padding:16px 22px}
  .ok{color:#7ee0c3}.bad{color:#f0807c}.warn{color:#e3c07b}
  .empty{color:#8b96a5;font-style:italic}
  footer{margin-top:36px;color:#8b96a5;font-size:12px;border-top:1px solid #232a35;padding-top:12px}
  code{font-family:ui-monospace,monospace;font-size:12px}
  @media print{body{background:#fff;color:#111}h2,a,.num{color:#0a7}.card{background:#f5f5f5}}
</style>
</head>
<body><main>
<h1>Досье: {{.Target}}</h1>
<div class="meta">сгенерировано {{.Generated.Format "02.01.2006 15:04 UTC"}} · прогон <code>{{.RunID}}</code></div>

<div class="cards">
{{if eq .Kind "username"}}
  <div class="card"><div class="num">{{len .Profiles}}</div><div class="meta">профилей найдено</div></div>
  <div class="card"><div class="num">{{.Checked}}</div><div class="meta">проверок по сайтам</div></div>
{{else}}
  <div class="card"><div class="num">{{len .Leaks}}</div><div class="meta">сайтов используют адрес</div></div>
  <div class="card"><div class="num">{{.Checked}}</div><div class="meta">сайтов проверено</div></div>
  <div class="card"><div class="num">{{.RateLimited}}</div><div class="meta">сайтов без ответа</div></div>
{{end}}
</div>

{{if eq .Kind "email"}}{{with .Email}}
<h2>Проверка адреса</h2>
<table>
{{if .HasSyntax}}<tr><th>Синтаксис</th><td>{{if .SyntaxValid}}<span class="ok">верный</span>{{else}}<span class="bad">неверный</span>{{end}}</td></tr>{{end}}
<tr><th>MX-записи</th><td>{{if .MX}}{{range $i, $m := .MX}}{{if $i}}, {{end}}<code>{{$m}}</code>{{end}}{{else}}<span class="warn">не найдены или DNS недоступен</span>{{end}}</td></tr>
{{if .HasDisposable}}<tr><th>Одноразовый домен</th><td>{{if .IsDisposable}}<span class="warn">да, временная почта</span>{{else}}<span class="ok">нет</span>{{end}}</td></tr>{{end}}
</table>
{{end}}{{end}}

{{if eq .Kind "username"}}
<h2>Профили</h2>
{{if .Profiles}}<table><tr><th>Сайт</th><th>Ссылка</th></tr>
{{range .Profiles}}<tr><td>{{.Site}}</td><td>{{if .URL}}<a href="{{.URL}}" rel="noopener">{{.URL}}</a>{{else}}—{{end}}</td></tr>{{end}}
</table>{{else}}<p class="empty">Профилей не найдено. Это тоже результат: проверенные сайты и метод перечислены ниже.</p>{{end}}
{{end}}

{{if or (eq .Kind "email") .Leaks .HasLeakSource}}
<h2>{{if eq .Kind "username"}}Записи об использовании ника{{else}}Сайты, где используется адрес{{end}}</h2>
{{if .Leaks}}<table><tr><th>Сайт</th><th>Домен</th></tr>
{{range .Leaks}}<tr><td>{{.Site}}</td><td>{{.Domain}}</td></tr>{{end}}
</table>{{else}}<p class="empty">Записей нет — ни один проверенный сайт не подтвердил адрес/ник. Различать «не найден» и «не проверен» видно по таблице источников ниже.</p>{{end}}
{{end}}

<h2>Источники</h2>
<table><tr><th>Источник</th><th>Статус</th><th>Проверено сайтов</th><th>Найдено</th><th>Причина</th></tr>
{{range .Sources}}<tr><td>{{.Title}}</td><td>{{if .OK}}<span class="ok">отработал</span>{{else}}<span class="bad">не отработал</span>{{end}}</td><td>{{if .Checked}}{{.Checked}}{{else}}—{{end}}</td><td>{{.Found}}</td><td>{{if .Reason}}<span class="warn">{{.Reason}}</span>{{else}}—{{end}}</td></tr>{{end}}
</table>
{{if gt .RateLimited 0}}<p class="meta">Часть сайтов ({{.RateLimited}}) ответила rate limit — по ним данных нет, и это видно в сырых данных.</p>{{end}}

<footer>
Отчёт собран движком fidespipe из прогона <code>{{.RunID}}</code>.
Приложены <code>evidence.json</code> (сырые выходы плагинов) и <code>journal.jsonl</code>
(журнал каждого шага): любую строку этого отчёта можно сверить с источником.
Если источник помечен ошибкой, его данных в отчёте нет — а не «ноль находок».
</footer>
</main></body></html>`))
