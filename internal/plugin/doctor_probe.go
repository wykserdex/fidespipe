package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wykserdex/fidespipe/internal/common"
)

// InterpreterInfo — ответ на вопрос «чем именно будут запускаться
// python-плагины».
type InterpreterInfo struct {
	// Path — интерпретатор, который вернул бы pythonInterpreter(), то есть
	// уже прошедший пробу на заглушку Microsoft Store.
	Path string `json:"path"`
	// Version — версия, как её печатает сам интерпретатор.
	Version string `json:"version"`
	// ScriptsDir — куда pip кладёт консольные скрипты этого интерпретатора.
	// Отдельным полем, потому что именно его отсутствие в PATH даёт картину
	// «пакет стоит, а команда не находится» — самую частую жалобу новичка.
	ScriptsDir string `json:"scripts_dir"`
	// ScriptsDirInPATH — есть ли этот каталог в PATH.
	ScriptsDirInPATH bool `json:"scripts_dir_in_path"`
}

// PythonInfo опрашивает окружение: находит интерпретатор тем же кодом, каким
// запускаются плагины, и спрашивает у него версию и путь к Scripts.
//
// Это ОПРОС, а не запуск плагина: ни один плагин не выполняется, ничего не
// ставится, сеть не используется. Интерпретатор приходится запустить — иначе
// ни версию, ни наличие пакета не узнать.
//
// Отдельная функция, а не расширение pythonInterpreter(), чтобы правило
// «спросить того же, кем запускаем» не разъезжалось по коду с тем местом,
// где плагины реально стартуют.
func PythonInfo() (InterpreterInfo, error) {
	py, err := pythonInterpreter()
	if err != nil {
		return InterpreterInfo{}, err
	}
	out, err := runProbe(py, `import sys, sysconfig
print(sys.version.split()[0])
print(sysconfig.get_path("scripts"))`)
	if err != nil {
		return InterpreterInfo{}, fmt.Errorf("интерпретатор %s найден, но сведения не отдал: %w", py, err)
	}
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	info := InterpreterInfo{Path: py, Version: strings.TrimSpace(lines[0])}
	if len(lines) > 1 {
		info.ScriptsDir = strings.TrimSpace(lines[1])
	}
	info.ScriptsDirInPATH = dirInPath(info.ScriptsDir)
	return info, nil
}

// dirInPath — есть ли каталог среди элементов PATH.
//
// Сравнение приведено к каноническому виду (Clean + сравнение без учёта
// регистра): на Windows PATH пишут то с завершающим обратным слэшем, то без,
// и «Scripts» и «Scripts\» — это один и тот же каталог.
func dirInPath(dir string) bool {
	if dir == "" {
		return false
	}
	target := strings.ToLower(filepath.Clean(dir))
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "" {
			continue
		}
		if strings.ToLower(filepath.Clean(p)) == target {
			return true
		}
	}
	return false
}

// PackageInfo — что интерпретатор знает о пакете.
//
// Importable и Installed — РАЗНЫЕ вопросы, и их смешение порождало ложный
// совет «pip install X» для уже установленного пакета:
//
//	importable — нашёлся ли модуль (find_spec): нужен плагину на паттерне C,
//	             который делает `python -c` со сниппетом;
//	installed  — есть ли дистрибутив (importlib.metadata): нужен для пакета,
//	             чей консольный скрипт запускается как бинарь из PATH.
//
// Пакет бывает установлен и не импортируем (нет верхнего модуля с таким
// именем) и наоборот. Скрипты отдельно: у maigret есть и то и другое, но его
// `maigret.exe` лежит в Scripts\, которого нет в PATH.
type PackageInfo struct {
	Importable bool `json:"importable"`
	Installed  bool `json:"installed"`
	// Version — версия дистрибутива, если он есть.
	Version string `json:"version,omitempty"`
	// Scripts — имена консольных скриптов пакета (entry_points
	// группы console_scripts). У exifread он называется EXIF.py, хотя пакет
	// зовут exifread, — поэтому сверять надо с ними, а не с именем пакета.
	Scripts []string `json:"scripts,omitempty"`
	// ProbeOK — удалось ли вообще опросить этот пакет. false означает «не
	// знаю», и это НЕ то же самое, что «не установлен»: без него совет
	// «pip install X» был бы выдумкой.
	ProbeOK bool `json:"probe_ok"`
}

// pythonProbeScript собирает всё нужное об одном пакете за ОДИН запуск
// интерпретатора.
//
// Имена приходят из манифестов, то есть из файлов, которые может править
// кто угодно, поэтому передаются через stdin, а не аргументами командной
// строки: так из них нельзя сделать фрагмент кода.
const pythonProbeScript = `import sys, json
try:
    import importlib.util as u
except Exception:
    sys.exit(3)
try:
    import importlib.metadata as md
except Exception:
    md = None
out = {}
for n in json.load(sys.stdin):
    rec = {"importable": False, "installed": False, "scripts": [], "probe_ok": True}
    try:
        rec["importable"] = u.find_spec(n) is not None
    except Exception:
        # Нет модуля с таким именем — это «не импортируется», а не «опрос
        # сломался»: родительский пакет может отсутствовать.
        rec["importable"] = False
    if md is not None:
        try:
            d = md.distribution(n)
            rec["installed"] = True
            rec["version"] = d.version
            rec["scripts"] = sorted(
                e.name for e in d.entry_points if e.group == "console_scripts")
        except Exception:
            rec["installed"] = False
    else:
        # Без importlib.metadata (Python < 3.8) про установку не знаем.
        rec["probe_ok"] = False
    out[n] = rec
print(json.dumps(out))`

// PythonPackages возвращает карту «имя → сведения». Отсутствие пакета — не
// ошибка: это и есть ответ. Ошибка возвращается только когда проба сама не
// отработала (нет интерпретатора, сломанный python, нечитаемый вывод).
func PythonPackages(py string, modules []string) (map[string]PackageInfo, error) {
	out := map[string]PackageInfo{}
	names := dedupeSorted(modules)
	if len(names) == 0 {
		return out, nil
	}
	if py == "" {
		return nil, fmt.Errorf("не указан интерпретатор для пробы пакетов")
	}
	stdin, err := json.Marshal(names)
	if err != nil {
		return nil, fmt.Errorf("подготовить список имён: %w", err)
	}
	cmd := exec.Command(py, "-X", "utf8", "-c", pythonProbeScript)
	cmd.Stdin = strings.NewReader(string(stdin))
	cmd.WaitDelay = common.ProcWaitDelay
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("проба пакетов не отработала (%s): %w", py, err)
	}
	var decoded map[string]PackageInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &decoded); err != nil {
		return nil, fmt.Errorf("проба пакетов вернула неразборчивый вывод: %w", err)
	}
	for name, info := range decoded {
		out[name] = info
	}
	return out, nil
}

// PythonModulesPresent — узкая обёртка поверх PythonPackages для тех, кому
// достаточно самого факта импортируемости.
//
// Оставлена, потому что на неё завязаны прежние вызовы и тесты; новая логика
// doctor использует PythonPackages.
func PythonModulesPresent(py string, modules []string) (map[string]bool, error) {
	info, err := PythonPackages(py, modules)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(info))
	for name, pi := range info {
		present[name] = pi.Importable
	}
	return present, nil
}

// runProbe выполняет короткий код на интерпретаторе и возвращает stdout.
//
// WaitDelay здесь — из того же соображения, что и при запуске плагинов: если
// проба что-то утечёт, ждать закрытия пайпов вечно нельзя.
func runProbe(py, code string) (string, error) {
	cmd := exec.Command(py, "-X", "utf8", "-c", code)
	cmd.WaitDelay = common.ProcWaitDelay
	out, err := common.Output(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// dedupeSorted убирает повторы и сортирует: порядок проб не должен зависеть от
// порядка обхода манифестов, иначе вывод doctor дрожит между запусками.
func dedupeSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
