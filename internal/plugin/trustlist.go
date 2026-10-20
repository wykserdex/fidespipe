package plugin

// Allow-list доверия к коду плагина.
//
// До 0.34 доверие объявлял САМ плагин: Manifest.Untrusted() читала поле
// `sandbox` из plugin.yaml, которое пишет автор плагина. Поле есть у НУЛЯ
// манифестов в репозитории, а internal/registry о нём не знает вовсе — то
// есть вредоносный community-плагин просто не писал строку `sandbox: untrusted`
// и получал права пользователя целиком.
//
// Теперь доверие даёт ЯДРО, и только по хэшу СОДЕРЖИМОГО каталога:
// AllowList.Allows(id, digest). Одного идентификатора мало — под именем
// official-плагина может лежать что угодно, поэтому проверяется и содержимое.
//
// Направление ошибки: любое сомнение (каталог не читается, symlink внутри,
// битый файл) даёт untrusted. Ложное срабатывание стоит лишней песочницы,
// ложное отсутствие — обхода доверия.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrDigestUnavailable — содержимое плагина не удалось посчитать.
// Fail-closed: плагин без проверяемого хэша доверия не получает.
var ErrDigestUnavailable = errors.New("не удалось вычислить хэш содержимого плагина")

// digestLockName — lock-файл установки. Единственное, что не входит в хэш.
//
// `.fidespipe` пишется в каталог плагина ПОСЛЕ копирования и содержит installed_at
// (время установки). Включённый в хэш, он сделал бы доверие невоспроизводимым:
// одна и та же установка давала бы разный хэш.
//
// Исключается только файл верхнего уровня, а не «любое имя на любой глубине», и
// только пока он не является исполняемым: см. entryIsHashed.
//
// ЧЕГО ЗДЕСЬ БОЛЬШЕ НЕТ (аудит N1). Раньше из хэша исключались ещё `.git`,
// `__pycache__` и любые `*.pyc`/`*.pyo` на любой глубине. Это был обход
// доверия: интерпретатор Python кладёт каталог скрипта первым в sys.path, и
// рядом с main.py достаточно положить `json.pyc` (sourceless-байткод), чтобы
// `import json` выполнил чужой код — при неизменном хэше. Аналогично
// `__pycache__/x.cpython-XY.pyc` с «unchecked hash» подменяет модуль x.py без
// проверки исходника. Всё, что может исполниться, обязано входить в хэш.
// Побочный кэш Python не должен появляться вовсе: плагины запускаются с
// PYTHONDONTWRITEBYTECODE=1 (process.go), поэтому официальный плагин не меняет
// собственный хэш между запусками.
const digestLockName = ".fidespipe"

// digestLockLegacyName — прежнее имя lock-файла. Исключается из хэша ТОЖЕ.
//
// Уже установленные плагины лежат со старым lock-файлом. Если он попадёт в
// хэш, хэш этих плагинов изменится — и allow-list оператора перестанет
// совпадать, то есть доверие сломается молча, ровно в момент переименования.
// Исключение обоих имён сохраняет хэш прежним.
const digestLockLegacyName = ".wedra"

// ignoredByName — не входит ли файл в хэш. rel — путь от корня плагина с
// прямыми слэшами.
func ignoredByName(rel string) bool {
	return rel == digestLockName || rel == digestLockLegacyName
}

// hasBytecode — есть ли в каталоге плагина байткод Python. Нужен только для
// диагностики: по такому каталогу хэш изменился, и оператору надо сказать, что
// именно удалить, а не оставить его наедине с «не в allow-list».
func hasBytecode(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if d.IsDir() && d.Name() == "__pycache__" {
			found = true
			return fs.SkipAll
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !d.IsDir() && (ext == ".pyc" || ext == ".pyo") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// entryIsHashed — точка входа плагина входит в хэш содержимого.
//
// Иначе manifest с `runtime.entry: .fidespipe` исполнял бы файл, исключённый из
// хэша. Путь, выходящий за каталог плагина, тоже не «содержимое» этого
// каталога. Проверка дублирует валидатор манифеста намеренно: решение о
// доверии не должно зависеть от того, что валидатор был вызван раньше.
func entryIsHashed(dir, entry string) bool {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return true // нет точки входа — нечего исполнять вне хэша
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(entry) || strings.HasPrefix(clean, "/") {
		return false
	}
	return !ignoredByName(clean)
}

// digestItem — файл, попавший в хэш.
type digestItem struct {
	rel  string
	size int64
	abs  string
}

// ContentDigest — sha256 содержимого каталога плагина в виде "sha256:<hex>".
//
// Считается по всем файлам каталога (рекурсивно) в порядке сортировки по
// относительному пути: иначе хэш зависел бы от порядка обхода ФС. В хэш входят
// относительный путь, размер и содержимое — иначе перестановка файлов или
// подмена одного другим того же размера дали бы тот же хэш.
//
// Возвращает ErrDigestUnavailable, если каталога нет, внутри symlink (он
// ссылается мимо каталога, и «содержимое» перестаёт быть тем, что исполняется)
// или какой-то файл не читается. Каталог — единственное место, где доверие
// подтверждается, поэтому «не смог посчитать» и «не доверен» — одно и то же.
func ContentDigest(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("%w: путь каталога пуст", ErrDigestUnavailable)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrDigestUnavailable, dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s не каталог", ErrDigestUnavailable, dir)
	}

	var items []digestItem
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrDigestUnavailable, path, err)
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return fmt.Errorf("%w: %s: %v", ErrDigestUnavailable, path, relErr)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// Symlink внутри каталога — не «файл, который мы не учли», а выход за
		// пределы проверяемого содержимого: хэш такого каталога был бы хэшем
		// ЧУЖОГО содержимого. Fail-closed, а не «посчитаем как есть».
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink внутри каталога плагина: %s", ErrDigestUnavailable, rel)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: не-обычный файл в каталоге плагина: %s", ErrDigestUnavailable, rel)
		}
		if ignoredByName(rel) {
			return nil
		}
		fi, statErr := d.Info()
		if statErr != nil {
			return fmt.Errorf("%w: %s: %v", ErrDigestUnavailable, path, statErr)
		}
		items = append(items, digestItem{rel: rel, size: fi.Size(), abs: path})
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}

	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })

	h := sha256.New()
	for _, it := range items {
		if _, err := io.WriteString(h, it.rel+"\x00"+fmt.Sprint(it.size)+"\x00"); err != nil {
			return "", fmt.Errorf("%w: %v", ErrDigestUnavailable, err)
		}
		f, openErr := os.Open(it.abs)
		if openErr != nil {
			return "", fmt.Errorf("%w: %s: %v", ErrDigestUnavailable, it.rel, openErr)
		}
		_, copyErr := io.Copy(h, f)
		f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("%w: %s: %v", ErrDigestUnavailable, it.rel, copyErr)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// AllowList — allow-list доверенных плагинов, который ведёт оператор ядра.
// Ключ — id плагина, значение — множество допустимых хэшей содержимого.
//
// Несколько хэшей на один id нужно из-за версий: официальный плагин, обновлённый
// релизом, доверен ровно настолько, насколько доверен конкретный его выпуск.
// Хэш не из allow-list означает «не доверен», а не «доверен, потому что id
// знаком» — иначе подмена содержимого проходила бы молча.
type AllowList struct {
	// byID — id → множество хэшей.
	byID map[string]map[string]struct{}
}

// NewAllowList — пустой allow-list. Ни один плагин не доверен.
func NewAllowList() *AllowList {
	return &AllowList{byID: map[string]map[string]struct{}{}}
}

// Allow — внести id@hash. Возвращает false, если запись уже была: повтор в
// конфиге не считается ошибкой, но и не должен молча задваивать память.
func (a *AllowList) Allow(id, hash string) bool {
	id = strings.TrimSpace(id)
	hash = strings.TrimSpace(hash)
	if id == "" || hash == "" {
		return false
	}
	if a.byID == nil {
		a.byID = map[string]map[string]struct{}{}
	}
	set, ok := a.byID[id]
	if !ok {
		set = map[string]struct{}{}
		a.byID[id] = set
	}
	if _, dup := set[hash]; dup {
		return false
	}
	set[hash] = struct{}{}
	return true
}

// AllowEntry — разобрать строку вида "id@sha256:<hex>" и внести её.
// Так выглядит запись в конфиге оператора: trusted_plugins: ["llm_openai@sha256:..."].
func (a *AllowList) AllowEntry(entry string) error {
	id, hash, err := SplitAllowEntry(entry)
	if err != nil {
		return err
	}
	a.Allow(id, hash)
	return nil
}

// Allows — доверен ли плагин с таким id и таким хэшем содержимого.
//
// Сравнение хэшей в постоянном времени не нужно: значение не секрет, оно
// публично лежит в конфиге оператора и выводится в диагностику.
func (a *AllowList) Allows(id, hash string) bool {
	if a == nil || a.byID == nil {
		return false
	}
	set, ok := a.byID[strings.TrimSpace(id)]
	if !ok {
		return false
	}
	_, ok = set[strings.TrimSpace(hash)]
	return ok
}

// Len — сколько id в allow-list (для диагностики и тестов).
func (a *AllowList) Len() int {
	if a == nil {
		return 0
	}
	return len(a.byID)
}

// IDs — отсортированный список доверенных id (для диагностики).
func (a *AllowList) IDs() []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, len(a.byID))
	for id := range a.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Merge — добавить все записи другого allow-list. nil-источник не трогает
// получателя: «нет второго списка» и «второй список пуст» дают один результат.
func (a *AllowList) Merge(other *AllowList) {
	if a == nil || other == nil {
		return
	}
	for id, set := range other.byID {
		for hash := range set {
			a.Allow(id, hash)
		}
	}
}

// Clone — независимая копия. Нужна там, где политика достраивается на ходу и
// исходный allow-list оператора трогать нельзя.
func (a *AllowList) Clone() *AllowList {
	out := NewAllowList()
	out.Merge(a)
	return out
}

// allowEntrySep — разделитель id и хэша в записи allow-list. '@' не встречается
// в id плагина (см. registry.ValidateComponent), поэтому он однозначен.
const allowEntrySep = "@"

// SplitAllowEntry — разобрать "id@sha256:<hex>".
//
// Строгая проверка формы здесь принципиальна: опечатка в allow-list (лишний
// пробел, потерянный префикс sha256:) обязана быть ВИДНА, а не превращаться в
// тихо неработающую запись, после которой плагин «почему-то» не доверен.
func SplitAllowEntry(entry string) (id, hash string, err error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", "", fmt.Errorf("пустая запись allow-list")
	}
	idx := strings.Index(entry, allowEntrySep)
	if idx <= 0 {
		return "", "", fmt.Errorf("запись allow-list %q: ожидается id@sha256:<hex>", entry)
	}
	id, hash = entry[:idx], entry[idx+len(allowEntrySep):]
	if err := ValidateAllowHash(hash); err != nil {
		return "", "", fmt.Errorf("запись allow-list %q: %w", entry, err)
	}
	return id, hash, nil
}

// ValidateAllowHash — хэш должен быть ровно "sha256:" + 64 hex-символа.
//
// Проверяется и длина, и алфавит: иначе опечатка дала бы запись, которая никогда
// не совпадёт с настоящим хэшем, и оператор искал бы причину не в конфиге.
func ValidateAllowHash(hash string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(hash, prefix) {
		return fmt.Errorf("хеш %q: ожидается префикс %q", hash, prefix)
	}
	hexPart := hash[len(prefix):]
	if len(hexPart) != 64 {
		return fmt.Errorf("хеш %q: ожидалось 64 hex-символа, получено %d", hash, len(hexPart))
	}
	for _, r := range hexPart {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			return fmt.Errorf("хеш %q: не-hex символ %q", hash, r)
		}
	}
	return nil
}

// TrustConfigFile — конфиг оператора с allow-list. Лежит рядом с проектом,
// как и registry.yaml: оператор правит его руками, ядро только читает.
//
// Формат:
//
//	version: "0.1"
//	trusted_plugins:
//	  - "syntax_mx_checker@sha256:<64 hex>"
//
// Конфиг НЕ обязателен: его отсутствие — не ошибка, а пустой allow-list (то
// есть «никто не доверен»). Ошибка парсинга, наоборот, ошибка: молча
// проигнорированный битый allow-list выглядел бы как «плагины перестали
// доверяться» без причины.
const TrustConfigFile = "fidespipe-trust.yaml"

// TrustConfigLegacyFile — прежнее имя файла конфига доверия.
//
// В нём лежат РЕШЕНИЯ оператора о доверии. Терять их из-за переименования
// нельзя: доверенные плагины перестали бы запускаться, и выглядело бы это как
// «плагин сломался», а не как «файл называется иначе». Поэтому прежнее имя
// поддерживается как запасное: нового файла нет — ищем старый.
const TrustConfigLegacyFile = "wedra-trust.yaml"

// TrustConfigFileExists — есть ли конфиг доверия по одному из имён.
//
// Сначала новое имя, потом прежнее: если оператор переименовал файл сам, его
// выбор должен уважаться.
func TrustConfigFileExists(dir string) (string, bool) {
	for _, name := range []string{TrustConfigFile, TrustConfigLegacyFile} {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

// TrustConfigVersion — версия формата конфига. Пустая версия допустима
// (файл с одним лишь trusted_plugins — нормальный конфиг), но несовпадение
// отвергается: иначе новый формат прочитался бы как старый.
const TrustConfigVersion = "0.1"

// trustConfig — структура файла оператора. KnownFields(true) в LoadTrustConfig
// отвергает неизвестные ключи: опечатка в ключе иначе сделала бы конфиг
// «пустым, но валидным», и доверие молча пропало бы.
type trustConfig struct {
	Version        string   `yaml:"version"`
	TrustedPlugins []string `yaml:"trusted_plugins"`
}

// LoadTrustConfig — прочитать конфиг оператора. Отсутствующий файл даёт
// пустой allow-list и nil-ошибку; нечитаемый/неверный — ошибку.
func LoadTrustConfig(path string) (*AllowList, error) {
	list := NewAllowList()
	if strings.TrimSpace(path) == "" {
		// Путь пуст не из-за ошибки чтения, а потому что оператор ничего не
		// настраивал: искать негде. Отличать это от «файла нет» не нужно —
		// результат тот же, пустой allow-list.
		return list, nil
	}
	if err := checkTrustConfigFile(path); err != nil {
		if os.IsNotExist(err) {
			return list, nil
		}
		return nil, fmt.Errorf("конфиг доверия %s небезопасен: %w", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return list, nil
		}
		return nil, fmt.Errorf("конфиг доверия %s: %w", path, err)
	}
	var cfg trustConfig
	if err := decodeStrictTrustConfig(raw, &cfg); err != nil {
		return nil, fmt.Errorf("конфиг доверия %s: %w", path, err)
	}
	if cfg.Version != "" && cfg.Version != TrustConfigVersion {
		return nil, fmt.Errorf("конфиг доверия %s: version %q, ожидается %q",
			path, cfg.Version, TrustConfigVersion)
	}
	for _, entry := range cfg.TrustedPlugins {
		if err := list.AllowEntry(entry); err != nil {
			return nil, fmt.Errorf("конфиг доверия %s: %w", path, err)
		}
	}
	return list, nil
}

// decodeStrictTrustConfig — разбор YAML с запретом неизвестных полей и
// запретом нескольких документов. Обе проверки обязательны по одной причине:
// конфиг доверия решает, чей код получит права пользователя, и «прочитал не то,
// что думал» здесь недопустимо.
func decodeStrictTrustConfig(raw []byte, cfg *trustConfig) error {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// Пустой файл — валидный «никто не доверен».
			return nil
		}
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("несколько YAML-документов")
		}
		return err
	}
	return nil
}

// BuiltinAllowList — встроенный allow-list, засеянный из пинов реестра.
//
// Отдельная копия при каждом вызове: возвращать общую карту нельзя, иначе
// вызывающий, добавивший запись для своего плагина, поправил бы доверие
// всех, кто пойдёт после него.
func BuiltinAllowList() *AllowList {
	list := NewAllowList()
	for id, hashes := range builtinTrusted {
		for _, h := range hashes {
			list.Allow(id, h)
		}
	}
	return list
}

// EffectiveAllowList — allow-list, по которому реально принимается решение:
// встроенный (пины реестра) ∪ конфиг оператора (fidespipe-trust.yaml).
//
// Объединение, а не замена: конфиг оператора дополняет встроенный список, и
// стирание из него засеянного плина обошлось бы в «все официальные плагины
// стали внешним кодом» — то есть в тихую поломку всех 23 реестровых плагинов на
// хостах без изолятора. Убрать доверие можно флагом --deny-untrusted-plugins
// или собственным sandbox: untrusted в манифесте, и оба способа не требуют
// правки сгенерированного файла.
//
// Отсутствие конфига — не ошибка: возвращается просто встроенный список.
func EffectiveAllowList(configPath string) (*AllowList, error) {
	list := BuiltinAllowList()
	operator, err := LoadTrustConfig(configPath)
	if err != nil {
		return nil, err
	}
	list.Merge(operator)
	return list, nil
}

// AllowListFromDirs — allow-list по каталогам, которые НАЗВАЛ оператор.
//
// Каталог трактуется как плагин, если в нём есть plugin.yaml; иначе — как корень
// с плагинами. Обход — на два уровня: plugins/<name> и plugins/official/<name>
// (плюс community/). Два уровня, а не один, потому что ровно эта раскладка
// принята в проекте (ScanPlugins, RefToDir, handlePlugins), и один уровень
// молча пропустил бы все official-плагины.
//
// Где это законно: `fidespipe plugin test <dir>`, конформность, тесты ядра. В этих
// местах каталог назвал человек (или тест, от имени того же человека) и попросил
// прогнать ИМЕННО ЕГО — это и есть решение о доверии, выраженное человеком, а не
// приписываемое плагином. Без этого разработка плагина была бы невозможна:
// `plugin test` по определению запускает чужой код по прямому указанию.
//
// Где это НЕ законно: путь, пришедший от манифеста плагина, из алиаса пайплайна
// или от агента. Там решение принимает ядро, и вызывающий обязан звать
// DecideTrust, а не AllowListFromDirs.
//
// Доверие выдаётся по хэшу содержимого, то есть ровно так же, как из
// fidespipe-trust.yaml: подмена файла после выдачи его отзовёт.
func AllowListFromDirs(dirs ...string) (*AllowList, error) {
	list := NewAllowList()
	eng := NewEngine()
	var add func(dir string, depth int)
	add = func(dir string, depth int) {
		if strings.TrimSpace(dir) == "" {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			// Отсутствующий каталог — штатный случай (фикстуры могут лежать не
			// по всем путям). Ошибку чтения существующего каталога глушать
			// нельзя, но и валить из-за неё весь вызов — тоже: каталог мог быть
			// не-каталогом плагина вовсе.
			return
		}
		if _, err := os.Stat(filepath.Join(dir, "plugin.yaml")); err == nil {
			addDirToAllowList(list, eng, dir)
			return
		}
		if depth <= 0 {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			add(filepath.Join(dir, e.Name()), depth-1)
		}
	}
	for _, dir := range dirs {
		add(dir, 2)
	}
	return list, nil
}

// addDirToAllowList — внести один каталог плагина.
func addDirToAllowList(list *AllowList, eng *Engine, dir string) error {
	m, err := eng.LoadManifest(dir)
	if err != nil {
		// Каталог без валидного манифеста пропускаем: AllowListFromDirs —
		// вспомогательная функция для каталогов, и отсутствие плагина здесь
		// не повод валить вызывающего.
		if m == nil {
			return nil
		}
	}
	if m == nil || m.ID == "" {
		return nil
	}
	digest, err := ContentDigest(m.Dir)
	if err != nil {
		return nil
	}
	list.Allow(m.ID, digest)
	return nil
}
