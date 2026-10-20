package api

// Тесты GET /api/plugins.
//
// Роут кормит палитру плагинов в редакторе (web/static/editor/app.js) и
// единственный вызов, которым пользуется человек. До этого у него не было НИ
// ОДНОГО теста: единственное упоминание /api/plugins в *_test.go было
// маркерной строкой в gui_embed_test.go. При этом список собирается в шести
// местах проекта, и расхождение между ними невозможно заметить без теста.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/plugin"
)

// writePluginDir кладёт каталог с манифестом. Пустой plugin.yaml достаточно:
// handlePlugins проверяет только наличие файла, а полную валидацию делает
// plugin validate — здесь она лишняя и замедлила бы тест.
func writePluginDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "id: " + name + "\nversion: 0.1.0\nplatform_api: \"0.1\"\n" +
		"runtime:\n  type: python\n  entry: main.py\ninput: {}\noutput: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// pluginsAPI — список плагинов от сервера. allowDirs (если заданы) получают
// явное доверие: см. trustfixture_test.go.
func pluginsAPI(t *testing.T, pluginsDir string, allowDirs ...string) []map[string]interface{} {
	t.Helper()
	// Ходим через настоящий httptest-сервер, а не через вызов хендлера
	// напрямую: так проверяется и код роута, и код ответа, как их увидит
	// браузер редактора.
	dir := t.TempDir()
	srv := NewServer(pluginsDir, filepath.Join(dir, "pipelines"), filepath.Join(dir, "runs"))
	for _, d := range allowDirs {
		trustManifestIn(t, srv, loadPluginForTrust(t, srv, d))
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/plugins = %d, ждали 200", resp.StatusCode)
	}
	var out []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("ответ не разбирается как JSON-массив: %v", err)
	}
	return out
}

func pluginIDs(list []map[string]interface{}) map[string]bool {
	ids := map[string]bool{}
	for _, p := range list {
		if id, ok := p["id"].(string); ok {
			ids[id] = true
		}
	}
	return ids
}

func TestAPIPluginsListsAllThreeRoots(t *testing.T) {
	root := t.TempDir()
	writePluginDir(t, root, "loose")
	writePluginDir(t, filepath.Join(root, "official"), "off")
	writePluginDir(t, filepath.Join(root, "community"), "com")

	got := pluginIDs(pluginsAPI(t, root))
	for _, want := range []string{"loose", "off", "com"} {
		if !got[want] {
			t.Errorf("плагин %q не попал в список: %v", want, got)
		}
	}
}

// Каталог без plugin.yaml — не плагин. Иначе в палитре появляются пустые
// серые прямоугольники, а validate потом ругается на отсутствующий манифест.
func TestAPIPluginsSkipsDirWithoutManifest(t *testing.T) {
	root := t.TempDir()
	writePluginDir(t, root, "real")
	if err := os.MkdirAll(filepath.Join(root, "not-a-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := pluginIDs(pluginsAPI(t, root))
	if got["not-a-plugin"] {
		t.Error("каталог без plugin.yaml попал в список плагинов")
	}
	if !got["real"] {
		t.Error("настоящий плагин потерялся")
	}
}

// Файл рядом с каталогами плагинов — не плагин, даже если он назван как
// плагин. Иначе одинокий plugin.yaml в корне plugins/ добавит в палитру
// запись, для которой нечего открывать.
func TestAPIPluginsSkipsFileThatLooksLikeManifest(t *testing.T) {
	root := t.TempDir()
	writePluginDir(t, root, "real")
	loose := filepath.Join(root, "stray")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loose, "plugin.yaml"),
		[]byte("id: stray\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := pluginIDs(pluginsAPI(t, root))
	if got["stray"] {
		t.Error("файл, не являющийся каталогом, попал в список плагинов")
	}
}

// Один и тот же плагин может лежать в корне и в official/. Дедуп по id
// обязателен: иначе палитра редактора покажет его дважды.
func TestAPIPluginsDedupesByID(t *testing.T) {
	root := t.TempDir()
	writePluginDir(t, root, "dupe")
	// Тот же id в official: манифест другой плагина с тем же именем.
	off := writePluginDir(t, filepath.Join(root, "official"), "dupe")
	if err := os.WriteFile(filepath.Join(off, "plugin.yaml"),
		[]byte("id: dupe\nversion: 0.2.0\nplatform_api: \"0.1\"\n"+
			"runtime:\n  type: python\n  entry: main.py\ninput: {}\noutput: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := pluginsAPI(t, root)
	count := 0
	for _, p := range list {
		if p["id"] == "dupe" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("плагин dupe встречается %d раз, ждали 1: %v", count, pluginIDs(list))
	}
}

func TestAPIPluginsEmptyRootIsNotAnError(t *testing.T) {
	got := pluginsAPI(t, t.TempDir())
	if len(got) != 0 {
		t.Errorf("пустой корень должен давать пустой список, получено %d", len(got))
	}
}

func TestAPIPluginsMissingRootIsNotFatal(t *testing.T) {
	// Отсутствующий каталог плагинов — обычное состояние (свежий checkout,
	// проект без плагинов). Роут обязан ответить 200 с пустым списком, а не
	// 500: иначе редактор не грузится вовсе.
	missing := filepath.Join(t.TempDir(), "no-such-plugins")
	got := pluginsAPI(t, missing)
	if len(got) != 0 {
		t.Errorf("несуществующий корень дал %d записей, ждали 0", len(got))
	}
}

// Плагин, которому ядро доверяет, не должен получать ни бейджа, ни причины:
// причина должна значить «что-то не так», а не «все плагины подозрительны».
//
// Тест переписан после инверсии доверия (H1). Раньше «обычным» считался любой
// плагин без строки `sandbox` в манифесте, и проверка «у него нет причины» была
// проверкой ровно того дырявого поведения, которое убрали. Теперь обычность —
// это не отсутствие признаков, а запись в allow-list по хэшу содержимого, и
// тест выдаёт её явно.
func TestAPIPluginsShowsAgentPluginsDirWithReason(t *testing.T) {
	root := t.TempDir()
	regular := writePluginDir(t, root, "regular")
	writePluginDir(t, filepath.Join(root, plugin.AgentPluginDir), "mailer")

	byID := map[string]map[string]interface{}{}
	for _, item := range pluginsAPI(t, root, trustPluginsDir(regular)) {
		id, _ := item["id"].(string)
		byID[id] = item
	}

	trusted, ok := byID["regular"]
	if !ok {
		t.Fatal("обычный плагин должен быть в списке")
	}
	if v, ok := trusted["agent_written"]; ok {
		t.Errorf("обычный плагин не должен помечаться как написанный агентом: %v", v)
	}
	if v, ok := trusted["blocked_reason"]; ok {
		t.Errorf("у доверенного плагина не должно быть причины отказа: %v", v)
	}
	if trusted["trusted"] != true {
		t.Errorf("доверенный плагин должен быть помечен trusted=true: %v", trusted)
	}

	mailer, ok := byID["mailer"]
	if !ok {
		t.Fatalf("плагин агента должен быть виден: %v", byID)
	}
	if mailer["agent_written"] != true {
		t.Errorf("плагин из %s обязан нести бейдж agent_written: %v",
			plugin.AgentPluginDir, mailer)
	}
	reason, _ := mailer["blocked_reason"].(string)
	if reason == "" {
		t.Fatal("плагин агента обязан нести причину: показать его и промолчать " +
			"о невозможности запуска — значит подсунуть кнопку, которая всегда откажет")
	}
	// Формулировка зависит от наличия изолятора, но суть одна: нужен изолятор.
	if !strings.Contains(reason, "изолятор") && !strings.Contains(reason, "изоляц") {
		t.Errorf("причина должна называть изолятор, а не быть общей фразой: %q", reason)
	}
	// Текст обязан оставаться правдой: он начинается с вердикта ядра, а не с
	// «плагин написан агентом» — после инверсии это лишь один из случаев.
	if !strings.Contains(reason, "агентом") {
		t.Errorf("причина должна называть вердикт ядра (плагин написан агентом): %q", reason)
	}
}

// pluginDetail — детальный вид одного плагина через настоящий httptest-сервер.
func pluginDetail(t *testing.T, pluginsDir, id string, allowDirs ...string) (map[string]interface{}, int) {
	t.Helper()
	dir := t.TempDir()
	srv := NewServer(pluginsDir, filepath.Join(dir, "pipelines"), filepath.Join(dir, "runs"))
	for _, d := range allowDirs {
		trustManifestIn(t, srv, loadPluginForTrust(t, srv, d))
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/plugins/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("детали плагина не разбираются как JSON: %v", err)
		}
	}
	return out, resp.StatusCode
}

// Детальный вид обязан говорить то же, что список: иначе оператор видит
// причину блокировки в одном месте и не видит в другом.
func TestAPIPluginDetailCarriesTrustFields(t *testing.T) {
	root := t.TempDir()
	regular := writePluginDir(t, root, "regular")
	writePluginDir(t, filepath.Join(root, plugin.AgentPluginDir), "mailer")

	got, code := pluginDetail(t, root, "regular", trustPluginsDir(regular))
	if code != http.StatusOK {
		t.Fatalf("детали обычного плагина: code=%d (want 200)", code)
	}
	if got["trusted"] != true {
		t.Errorf("детали должны помечать доверенный плагин trusted=true: %v", got)
	}
	if _, ok := got["blocked_reason"]; ok {
		t.Errorf("у доверенного плагина не должно быть причины отказа: %v", got)
	}

	mailer, code := pluginDetail(t, root, "mailer", trustPluginsDir(regular))
	if code != http.StatusOK {
		t.Fatalf("детали плагина агента: code=%d (want 200)", code)
	}
	if mailer["agent_written"] != true {
		t.Errorf("детали должны нести бейдж agent_written: %v", mailer)
	}
	reason, _ := mailer["blocked_reason"].(string)
	if reason == "" {
		t.Fatal("детали недоверенного плагина обязаны нести причину отказа")
	}
}

// Плагин, КОТОРОГО НЕТ в allow-list, обязан быть помечен как недоверенный и
// получить причину — даже если он не написан агентом и молчит в манифесте.
//
// Это ровно тот случай, который после инверсии стал главным: обычный
// community-плагин без строки `sandbox` раньше получал права пользователя
// целиком. Молчать о нём значит подсунуть кнопку, которая всегда откажет.
func TestAPIPluginsMarksUnlistedPluginWithReason(t *testing.T) {
	root := t.TempDir()
	writePluginDir(t, root, "unlisted")

	byID := map[string]map[string]interface{}{}
	for _, item := range pluginsAPI(t, root) {
		id, _ := item["id"].(string)
		byID[id] = item
	}
	got, ok := byID["unlisted"]
	if !ok {
		t.Fatalf("плагин должен быть в списке: %v", byID)
	}
	if got["trusted"] != false {
		t.Errorf("плагин без записи в allow-list обязан быть trusted=false: %v", got)
	}
	reason, _ := got["blocked_reason"].(string)
	if reason == "" {
		t.Fatal("недоверенный плагин обязан нести причину отказа")
	}
	if !strings.Contains(reason, "allow-list") {
		t.Errorf("причина должна называть allow-list: %q", reason)
	}
	if _, ok := got["content_sha256"]; !ok {
		t.Error("список должен показывать хэш содержимого — иначе оператору нечего вносить в allow-list")
	}
}
