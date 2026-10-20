package plugin

// Инверсия доверия (H1) — обязательные тесты.
//
// Исходная находка: доверие объявлял сам плагин. Manifest.Untrusted() читала
// поле `sandbox` из plugin.yaml, которого нет ни в одном из 54 манифестов
// репозитория и о котором internal/registry не знает вовсе. Вредоносный
// community-плагин мог просто не написать эту строку — и получить права
// пользователя целиком.
//
// Тесты ниже закрывают пять сторон инверсии:
//   1. Плагин извне без поля sandbox не доверен.
//   2. Манифест не может повысить доверие — ни через какое поле.
//   3. Совпавший хэш → доверен; подмена файла → не доверен.
//   4. Без изолятора недоверенный плагин не запускается (fail closed).
//   5. Symlink/«..» в пути к плагину не выдаёт доверия.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// writePluginDir — каталог плагина с заданным main.py и манифестом.
// sandbox передаётся как есть: пустая строка означает «плагин не писал поле»,
// то есть ровно тот случай, который раньше выдавал полные права.
func writePluginDir(t *testing.T, id, sandbox, script string) *pipeline.Manifest {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := "id: " + id + "\nversion: 0.1.0\nplatform_api: \"0.1\"\n" +
		"runtime:\n  type: python\n  entry: main.py\n"
	if sandbox != "" {
		manifest += "sandbox: " + sandbox + "\n"
	}
	manifest += "input: {}\noutput: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return &pipeline.Manifest{
		ID:      id,
		Version: "0.1.0",
		Runtime: pipeline.Runtime{Type: "python", Entry: "main.py"},
		Sandbox: sandbox,
		Dir:     dir,
	}
}

const okScript = "import sys\nsys.stdout.write('{\"status\":\"ok\",\"output\":{}}')\n"

// ── 1. Плагин извне без поля sandbox не доверен ────────────────────────────

// ГЛАВНЫЙ ТЕСТ ИНВЕРСИИ: плагин, скачанный извне, без строки `sandbox`
// больше не доверен.
//
// Раньше именно этот случай проходил как доверенный, и это была дыра: поле
// писал автор плагина, поэтому отсутствие поля означало «доверен».
func TestExternalPluginWithoutSandboxFieldIsNotTrusted(t *testing.T) {
	m := writePluginDir(t, "evil", "", okScript)
	if m.Sandbox != "" {
		t.Fatalf("фикстура должна быть без поля sandbox, а не %q", m.Sandbox)
	}
	decision := DecideTrust(m, TrustPolicy{Trusted: NewAllowList()})
	if decision.Trusted {
		t.Fatal("плагин извне без записи в allow-list доверен — это и есть находка H1")
	}
	if !strings.Contains(decision.Reason, "allow-list") {
		t.Errorf("причина должна называть allow-list, а не быть общей фразой: %q", decision.Reason)
	}
}

// Тот же вердикт обязан быть и при пустой политике (Exec без ctx). Раньше пустой
// политикой был «доверен по умолчанию», и забытый флаг тихо выдавал права.
func TestEmptyPolicyTrustsNobody(t *testing.T) {
	m := writePluginDir(t, "evil", "", okScript)
	if IsTrusted(m, TrustPolicy{}) {
		t.Fatal("пустая политика обязана быть fail-closed: никто не доверен")
	}
	if IsTrusted(m, TrustPolicy{Trusted: nil}) {
		t.Fatal("nil allow-list обязан означать «никто не доверен», а не «все доверены»")
	}
}

// Реальный запуск: без согласия оператора процесс не создаётся, даже если
// манифест молчит. Проверяется маркер на диске, а не только код ошибки —
// «ошибка» без маркера может означать что угодно.
func TestExternalPluginDoesNotRunWithoutConsent(t *testing.T) {
	requirePython(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := "open(" + quotePy(marker) + ", 'w').write('ran')\n" +
		"import sys\nsys.stdout.write('{\"status\":\"ok\",\"output\":{}}')\n"
	m := writePluginDir(t, "evil", "", script)
	m.Dir = dir
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	res := Exec(m, []byte("{}"), 5*time.Second)
	if res.OK() {
		t.Fatal("плагин извне запустился без согласия оператора")
	}
	if res.ErrCode != "sandbox_unavailable" {
		t.Fatalf("ErrCode = %q, ожидался sandbox_unavailable", res.ErrCode)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("процесс плагина был запущен — маркер создан")
	}
}

// ── 2. Манифест не может повысить доверие ──────────────────────────────────

// ИНВАРИАНТ: манифест вправе только ПОНИЗИТЬ доверие.
//
// Перебираются все поля, которыми плагин мог бы себя «обосновать». Пока
// перечисление полное, добавление нового поля в манифест обязано ломать этот
// тест — иначе новое поле может оказаться ещё одним каналом повышения доверия,
// о котором никто не подумал.
func TestManifestCannotRaiseTrust(t *testing.T) {
	m := writePluginDir(t, "evil", "", okScript)
	digest, err := ContentDigest(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	// Даже когда allow-list СОДЕРжит этот плагин, манифест не может вывести его
	// из состояния «недоверен» — он может только туда завести.
	trusted := NewAllowList()
	trusted.Allow(m.ID, digest)

	downgrades := []struct {
		name string
		mut  func(*pipeline.Manifest)
	}{
		{"sandbox: untrusted", func(x *pipeline.Manifest) { x.Sandbox = pipeline.SandboxUntrusted }},
		{"sandbox: trusted", func(x *pipeline.Manifest) { x.Sandbox = pipeline.SandboxTrusted }},
		{"автор", func(x *pipeline.Manifest) { x.Author = "Fidespipe Security Team" }},
		{"platform_api", func(x *pipeline.Manifest) { x.PlatformAPI = "0.2" }},
		{"версия 999", func(x *pipeline.Manifest) { x.Version = "999.0.0" }},
		{"секреты", func(x *pipeline.Manifest) { x.Permissions.Secrets = []string{"HOME"} }},
		{"сеть any_host", func(x *pipeline.Manifest) {
			x.Permissions.Network = []pipeline.NetworkPermission{{AnyHost: true, Port: 443}}
		}},
		{"filesystem", func(x *pipeline.Manifest) { x.Permissions.Filesystem = "workspace" }},
		{"runtime.requires", func(x *pipeline.Manifest) { x.Runtime.Requires = []string{"python==3.12"} }},
	}
	// Пустой allow-list: проверяем, что ни одно поле не превращает
	// недоверенный плагин в доверенный.
	for _, c := range downgrades {
		probe := *m
		c.mut(&probe)
		if IsTrusted(&probe, TrustPolicy{Trusted: NewAllowList()}) {
			t.Errorf("поле %q выдало доверие плагину, которого нет в allow-list", c.name)
		}
	}
	// И обратная сторона: тот же набор полей НЕ может выбить плагин из
	// allow-list, если там его понизили через sandbox.
	for _, c := range downgrades {
		probe := *m
		c.mut(&probe)
		probe.Sandbox = pipeline.SandboxUntrusted
		if IsTrusted(&probe, TrustPolicy{Trusted: trusted}) {
			t.Errorf("поле %q отменило понижение доверия манифестом", c.name)
		}
	}
}

// Понижение манифестом сильнее allow-list: оператор мог внести плагин в
// список по старому хэшу, а автор позже пометил его как внешний код.
func TestManifestDowngradeBeatsAllowList(t *testing.T) {
	m := writePluginDir(t, "honest", pipeline.SandboxUntrusted, okScript)
	digest, err := ContentDigest(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	trusted := NewAllowList()
	trusted.Allow(m.ID, digest)

	decision := DecideTrust(m, TrustPolicy{Trusted: trusted})
	if decision.Trusted {
		t.Fatal("sandbox: untrusted обязан понижать доверие даже при записи в allow-list")
	}
	if !strings.Contains(decision.Reason, "sandbox: untrusted") {
		t.Errorf("причина должна называть поле манифеста: %q", decision.Reason)
	}
}

// Плагин агента остаётся внешним кодом по построению — появление его в
// allow-list этого не меняет (он не был там по построению же, и запись в
// allow-list выдаётся ядром оператору, а не генерируется автоматически).
func TestAgentWrittenPluginStaysUntrusted(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, AgentPluginDir, "mailer"), 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(dir, AgentPluginDir, "mailer")
	if err := os.WriteFile(filepath.Join(inner, "main.py"), []byte(okScript), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &pipeline.Manifest{
		ID: "mailer", Version: "0.1.0",
		Runtime: pipeline.Runtime{Type: "python", Entry: "main.py"},
		Dir:     inner,
	}
	digest, err := ContentDigest(inner)
	if err != nil {
		t.Fatal(err)
	}
	trusted := NewAllowList()
	trusted.Allow(m.ID, digest)
	if IsTrusted(m, TrustPolicy{Trusted: trusted}) {
		t.Fatal("плагин из agent-plugins/ обязан остаться внешним кодом по построению")
	}
}

// ── 3. Хэш содержимого: совпал → доверен, подмена → не доверен ───────────

// Официальный плагин, содержимое которого совпадает с записью в allow-list,
// доверен. Обратная сторона инверсии обязана быть закреплена тестом: иначе
// «доверен = всегда в песочнице» прошло бы незамеченным.
func TestOfficialPluginWithMatchingHashIsTrusted(t *testing.T) {
	m := writePluginDir(t, "llm_openai", "", okScript)
	digest, err := ContentDigest(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	trusted := NewAllowList()
	trusted.Allow(m.ID, digest)

	decision := DecideTrust(m, TrustPolicy{Trusted: trusted})
	if !decision.Trusted {
		t.Fatalf("плагин с совпавшим хэшем должен быть доверен: %+v", decision)
	}
}

// ТЕСТ НА ПОДМЕНУ ФАЙЛА: после установки (то есть после выдачи доверия) файл
// плагина изменён — доверие обязано отозваться.
//
// Это ровно тот случай, ради которого проверяется содержимое, а не id: без
// проверки хэша под именем official-плагина лежал бы любой код.
func TestFileSubstitutionRevokesTrust(t *testing.T) {
	m := writePluginDir(t, "llm_openai", "", okScript)
	digest, err := ContentDigest(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	trusted := NewAllowList()
	trusted.Allow(m.ID, digest)
	if !IsTrusted(m, TrustPolicy{Trusted: trusted}) {
		t.Fatal("до подмены плагин должен быть доверен")
	}

	// Подмена содержимого на месте — без смены id и имени файла.
	evil := "import os,sys\n" +
		"open(" + quotePy(filepath.Join(t.TempDir(), "stolen")) + ", 'w').write(os.environ.get('HOME',''))\n" +
		"sys.stdout.write('{\"status\":\"ok\",\"output\":{}}')\n"
	if err := os.WriteFile(filepath.Join(m.Dir, "main.py"), []byte(evil), 0o600); err != nil {
		t.Fatal(err)
	}

	decision := DecideTrust(m, TrustPolicy{Trusted: trusted})
	if decision.Trusted {
		t.Fatal("подмена файла обязана понизить доверие — иначе проверка по id бесполезна")
	}
	if decision.Digest == digest {
		t.Fatal("хеш содержимого не изменился после подмены — он не покрывает содержимое файлов")
	}
}

// Реальный запуск подменённого плагина: процесс не создаётся.
func TestSubstitutedPluginDoesNotRun(t *testing.T) {
	requirePython(t)
	m := writePluginDir(t, "llm_openai", "", okScript)
	digest, err := ContentDigest(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	trusted := NewAllowList()
	trusted.Allow(m.ID, digest)

	marker := filepath.Join(t.TempDir(), "stolen")
	evil := "open(" + quotePy(marker) + ", 'w').write('x')\n" +
		"import sys\nsys.stdout.write('{\"status\":\"ok\",\"output\":{}}')\n"
	if err := os.WriteFile(filepath.Join(m.Dir, "main.py"), []byte(evil), 0o600); err != nil {
		t.Fatal(err)
	}

	res := ExecWithEnvCtx(WithTrustPolicy(context.Background(), TrustPolicy{Trusted: trusted}),
		m, []byte("{}"), 5*time.Second, nil)
	if res.OK() {
		t.Fatal("подменённый плагин запустился")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("процесс подменённого плагина был запущен — маркер создан")
	}
}

// Добавление ЛИШНЕГО файла в доверенный каталог тоже понижает доверие: иначе
// злоумышленник просто дописал бы рядом свой модуль и импортировал его.
func TestExtraFileRevokesTrust(t *testing.T) {
	m := writePluginDir(t, "helper", "", okScript)
	digest, err := ContentDigest(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	trusted := NewAllowList()
	trusted.Allow(m.ID, digest)
	if !IsTrusted(m, TrustPolicy{Trusted: trusted}) {
		t.Fatal("до добавления файла плагин должен быть доверен")
	}
	if err := os.WriteFile(filepath.Join(m.Dir, "extra.py"), []byte("x=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsTrusted(m, TrustPolicy{Trusted: trusted}) {
		t.Fatal("добавление файла в доверенный каталог обязано понизить доверие")
	}
}

// ── 4. Fail closed там, где изолятора нет ────────────────────────────────

// Платформа без изолятора обязана ОТКАЗЫВАТЬ, а не выполнять код с правами
// пользователя. Проверяется маркером: «ошибка» сама по себе ничего не значит.
func TestUntrustedPluginDoesNotRunWithoutIsolator(t *testing.T) {
	if SandboxUsable() {
		t.Skip("на этой машине изолятор есть — проверка fail-closed неприменима")
	}
	requirePython(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := "open(" + quotePy(marker) + ", 'w').write('ran')\n" +
		"import sys\nsys.stdout.write('{\"status\":\"ok\",\"output\":{}}')\n"
	m := writePluginDir(t, "evil", "", script)
	m.Dir = dir
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	// Даже с явным согласием оператора: согласие разрешает ВНЕШНИЙ код, а
	// внешний код без изолятора исполнять нечем. Это и есть fail-closed.
	res := ExecWithEnvCtx(AllowUntrustedPlugins(context.Background()), m, []byte("{}"), 5*time.Second, nil)
	if res.OK() {
		t.Fatalf("на %s без изолятора внешний код исполнен: %+v", runtime.GOOS, res)
	}
	if res.ErrCode != "sandbox_unavailable" {
		t.Fatalf("ErrCode = %q, ожидался sandbox_unavailable", res.ErrCode)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("процесс запущен без изолятора — маркер создан")
	}
}

// ── 5. Обход через путь: symlink и «..» не выдают доверия ───────────────

// Симлинк ВНУТРИ каталога доверенного плагина: хэш такого каталога — это уже
// не хэш его содержимого (часть файлов лежит вне), поэтому доверие не
// выдаётся. Направление ошибки выбрано в сторону untrusted: лишняя песочница
// дешевле обхода.
func TestSymlinkInsidePluginDirRevokesTrust(t *testing.T) {
	m := writePluginDir(t, "helper", "", okScript)
	digest, err := ContentDigest(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	trusted := NewAllowList()
	trusted.Allow(m.ID, digest)
	if !IsTrusted(m, TrustPolicy{Trusted: trusted}) {
		t.Fatal("до подмены плагин должен быть доверен")
	}

	// Подменяем main.py симлинком на файл, которого в каталоге нет. Хэш
	// доверенного каталога такой записи не описывает — значит, доверять нельзя.
	secret := filepath.Join(t.TempDir(), "secret.py")
	if err := os.WriteFile(secret, []byte(okScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(m.Dir, "main.py")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(m.Dir, "main.py")); err != nil {
		t.Skipf("симлинки недоступны (%v)", err)
	}

	if _, err := ContentDigest(m.Dir); err == nil {
		t.Fatal("ContentDigest обязан отвергать каталог с симлинком — иначе доверие выдаётся чужому содержимому")
	}
	if IsTrusted(m, TrustPolicy{Trusted: trusted}) {
		t.Fatal("симлинк внутри каталога обязан отозвать доверие")
	}
}

// Плагин, путь к которому уводит «..» В СТОРОННЕЙ доверенный каталог, не
// должен получать доверие «по имени каталога».
//
// Проверяется ровно то, что выглядит обходом: каталог плагина физически
// другой, а ссылка ведёт в доверенное содержимое. Решение обязано опираться на
// СОДЕРЖИМОЕ и на id, а не на то, куда указывает путь.
func TestDotDotPathDoesNotBorrowTrust(t *testing.T) {
	base := t.TempDir()
	_, realTrusted := writeTrustedPluginDir(t, base, "real")

	// Каталог-обманка: путь уводит «..» в доверенный каталог, а id другой.
	deceptive := &pipeline.Manifest{
		ID:      "evil",
		Version: "0.1.0",
		Runtime: pipeline.Runtime{Type: "python", Entry: "main.py"},
		Dir:     filepath.Join(base, "evil", "..", "real"),
	}
	if IsTrusted(deceptive, TrustPolicy{Trusted: realTrusted}) {
		t.Fatal("чужой id, достигающий доверенного содержимого через «..», получил доверие")
	}
}

// Симлинк-каталог на доверенное содержимое — тоже не доверенный плагин:
// доверен конкретный id конкретного каталога, а не всё, что на него
// указывает.
//
// Отдельно от теста с «..», потому что симлинки на Windows требуют Developer
// Mode или прав администратора: объединённый тест молча пропускал бы и
// проверку «..» на этой платформе.
func TestSymlinkedDirDoesNotBorrowTrust(t *testing.T) {
	base := t.TempDir()
	realDir, realTrusted := writeTrustedPluginDir(t, base, "real")

	linkDir := filepath.Join(base, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("симлинки недоступны на %s: %v", runtime.GOOS, err)
	}
	byLink := &pipeline.Manifest{
		ID:      "evil",
		Version: "0.1.0",
		Runtime: pipeline.Runtime{Type: "python", Entry: "main.py"},
		Dir:     linkDir,
	}
	if IsTrusted(byLink, TrustPolicy{Trusted: realTrusted}) {
		t.Fatal("плагин, чей каталог — симлинк на доверенный, получил доверие чужого id")
	}
}

// writeTrustedPluginDir — доверенный плагин в base: возвращает каталог и
// allow-list, в который он внесён.
func writeTrustedPluginDir(t *testing.T, base, id string) (dir string, trusted *AllowList) {
	t.Helper()
	dir = filepath.Join(base, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(okScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"),
		[]byte("id: "+id+"\nversion: 0.1.0\nplatform_api: \"0.1\"\n"+
			"runtime:\n  type: python\n  entry: main.py\ninput: {}\noutput: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := ContentDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	trusted = NewAllowList()
	trusted.Allow(id, digest)
	return dir, trusted
}

// Каталог, которого нет, — не доверен (fail-closed), даже если id красивый.
func TestMissingDirIsNotTrusted(t *testing.T) {
	m := &pipeline.Manifest{ID: "ghost", Dir: filepath.Join(t.TempDir(), "no-such-dir")}
	decision := DecideTrust(m, TrustPolicy{Trusted: NewAllowList()})
	if decision.Trusted {
		t.Fatal("несуществующий каталог не может быть доверенным")
	}
	if decision.Digest != "" {
		t.Fatalf("при нечитаемом содержимом хэш должен быть пустым, получено %q", decision.Digest)
	}
}
