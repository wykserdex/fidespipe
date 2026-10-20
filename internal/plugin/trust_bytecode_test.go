package plugin

// Регрессии аудита N1: хэш содержимого плагина обязан покрывать всё, что может
// исполниться. Раньше `*.pyc`, `__pycache__` и файл `.fidespipe` на любой глубине
// исключались из хэша, и рядом с main.py можно было положить `json.pyc` —
// `import json` выполнял чужой код, а плагин оставался доверенным.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

const jsonImportingScript = "import json, sys\n" +
	"json.load(sys.stdin)\n" +
	"print(json.dumps({'status': 'ok', 'output': {}}))\n"

func mustDigest(t *testing.T, dir string) string {
	t.Helper()
	d, err := ContentDigest(dir)
	if err != nil {
		t.Fatalf("ContentDigest(%s): %v", dir, err)
	}
	return d
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Любой добавленный файл, который интерпретатор может загрузить, меняет хэш.
func TestContentDigestCoversEverythingExecutable(t *testing.T) {
	cases := []struct{ name, rel string }{
		{"sourceless json.pyc рядом с main.py", "json.pyc"},
		{"оптимизированный .pyo", "re.pyo"},
		{"pyc в __pycache__", "__pycache__/helper.cpython-313.pyc"},
		{"pyc в верхнем регистре расширения", "json.PYC"},
		{"файл .fidespipe во вложенном каталоге", "sub/.fidespipe"},
		{"файл .git в корне", ".git"},
		{"файл внутри .git", ".git/hook.py"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, dir := writePlainPlugin(t, "p", jsonImportingScript)
			before := mustDigest(t, dir)
			writeFile(t, filepath.Join(dir, filepath.FromSlash(tc.rel)), "EVIL")
			if after := mustDigest(t, dir); after == before {
				t.Fatalf("добавление %s не изменило хэш — обход доверия", tc.rel)
			}
		})
	}
}

// Lock-файл установки по-прежнему вне хэша — иначе доверие невоспроизводимо.
func TestContentDigestIgnoresOnlyTopLevelLockFile(t *testing.T) {
	_, dir := writePlainPlugin(t, "p", jsonImportingScript)
	before := mustDigest(t, dir)
	writeFile(t, filepath.Join(dir, ".fidespipe"), "installed_at: 2026-01-01\n")
	if mustDigest(t, dir) != before {
		t.Fatal("lock-файл .fidespipe в корне не должен менять хэш")
	}
	writeFile(t, filepath.Join(dir, ".fidespipe"), "installed_at: 2030-01-01\n")
	if mustDigest(t, dir) != before {
		t.Fatal("содержимое lock-файла не должно влиять на хэш")
	}
}

// Решение о доверии: официальный по виду плагин + подложенный json.pyc → не
// доверен, и причина называет байткод.
func TestDecideTrustRevokedByInjectedBytecode(t *testing.T) {
	m, dir := writePlainPlugin(t, "p", jsonImportingScript)
	policy := TrustPolicy{Trusted: allowFor(t, m)}
	if !DecideTrust(m, policy).Trusted {
		t.Fatal("контрольный случай: плагин должен быть доверен до подмены")
	}
	writeFile(t, filepath.Join(dir, "json.pyc"), "EVIL")
	got := DecideTrust(m, policy)
	if got.Trusted {
		t.Fatal("плагин с подложенным json.pyc остался доверенным (N1)")
	}
	if !strings.Contains(got.Reason, "байткод") {
		t.Errorf("причина должна говорить про байткод, получено: %s", got.Reason)
	}
}

func allowFor(t *testing.T, m *pipeline.Manifest) *AllowList {
	t.Helper()
	l := NewAllowList()
	l.Allow(m.ID, mustDigest(t, m.Dir))
	return l
}

// Точка входа вне хэша не доверяется, даже если хэш остальных файлов совпал.
func TestDecideTrustRejectsEntryOutsideDigest(t *testing.T) {
	for _, entry := range []string{".fidespipe", "../other/main.py", "/etc/passwd", ".", ".."} {
		m, dir := writePlainPlugin(t, "p", jsonImportingScript)
		m.Runtime.Entry = entry
		writeFile(t, filepath.Join(dir, ".fidespipe"), "print('not hashed')\n")
		policy := TrustPolicy{Trusted: allowFor(t, m)}
		got := DecideTrust(m, policy)
		if got.Trusted {
			t.Errorf("entry %q: плагин с точкой входа вне хэша не должен быть доверен", entry)
		}
		if !strings.Contains(got.Reason, "runtime.entry") {
			t.Errorf("entry %q: причина должна называть runtime.entry: %s", entry, got.Reason)
		}
	}
	m, _ := writePlainPlugin(t, "p", jsonImportingScript)
	if !DecideTrust(m, TrustPolicy{Trusted: allowFor(t, m)}).Trusted {
		t.Error("обычный entry main.py должен оставаться доверенным")
	}
}

// Сквозной: плагин, импортирующий json, с подложенным json.pyc не запускается
// вовсе (untrusted без согласия оператора), и подложенный код не выполняется.
func TestExecDoesNotRunInjectedBytecode(t *testing.T) {
	requirePython(t)
	m, dir := writePlainPlugin(t, "p", jsonImportingScript)
	ctx := trustCtx(t, m) // доверие выдано по хэшу ЧИСТОГО каталога

	marker := filepath.Join(t.TempDir(), "pwned")
	evil := filepath.Join(t.TempDir(), "json_evil.py")
	writeFile(t, evil, "open("+quotePy(marker)+", 'w').write('x')\nimport sys\nsys.exit(0)\n")
	compileSourceless(t, evil, filepath.Join(dir, "json.pyc"))

	res := ExecWithEnvCtx(ctx, m, []byte("{}"), 10*time.Second, nil)
	if res.OK() {
		t.Fatalf("плагин с подложенным json.pyc выполнился: %+v", res)
	}
	// Отказ обязан прийти из enforceTrust (нет согласия оператора), а не из
	// сборки песочницы: код sandbox_unavailable у них общий, и без проверки
	// текста тест проходил бы «по другой причине» на хосте без bwrap и
	// молча переставал бы проверять доверие на хосте с ним.
	if res.ErrCode != "sandbox_unavailable" || !strings.Contains(res.ErrMsg, "нет согласия на запуск внешнего кода") {
		t.Errorf("ожидался отказ enforceTrust (нет согласия оператора), получено %q: %s", res.ErrCode, res.ErrMsg)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("подложенный байткод исполнился (создан маркер)")
	}
}

// Доверенный плагин с локальным модулем не меняет хэш от запуска к запуску:
// байткод не пишется (PYTHONDONTWRITEBYTECODE), значит исключать __pycache__
// из хэша не нужно.
func TestTrustedPluginWithLocalModuleKeepsDigestAcrossRuns(t *testing.T) {
	requirePython(t)
	m, dir := writePlainPlugin(t, "p",
		"import sys, json\nimport helper\njson.load(sys.stdin)\nprint(json.dumps({'status':'ok','output':{'v': helper.VALUE}}))\n")
	writeFile(t, filepath.Join(dir, "helper.py"), "VALUE = 7\n")
	before := mustDigest(t, dir)
	ctx := trustCtx(t, m)
	for i := 0; i < 2; i++ {
		res := ExecWithEnvCtx(ctx, m, []byte("{}"), 10*time.Second, nil)
		if !res.OK() {
			t.Fatalf("запуск %d: плагин не выполнился: %+v", i+1, res)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "__pycache__")); err == nil {
		t.Error("плагин создал __pycache__ внутри своего каталога")
	}
	if after := mustDigest(t, dir); after != before {
		t.Errorf("хэш изменился после запусков: %s → %s", before, after)
	}
}

// compileSourceless — скомпилировать src в sourceless .pyc по пути dst.
func compileSourceless(t *testing.T, src, dst string) {
	t.Helper()
	out, err := exec.Command("python3", "-c",
		"import py_compile,sys; py_compile.compile(sys.argv[1], cfile=sys.argv[2], doraise=True)", src, dst).CombinedOutput()
	if err != nil {
		t.Fatalf("компиляция байткода: %v: %s", err, out)
	}
}
