package registry

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wykserdex/fidespipe/internal/common"
)

// Registry v0.1 — формат заморозить как протокол.
// Файл registry.yaml лежит в корне репозитория-реестра.
//
// Плагин/пресет = запись (Entry): source (git-репо), path (подпуть),
// version (тег/ветка). Установленный плагин живёт в <pluginsDir>/<name>
// и несёт lock-файл .fidespipe с версией (для пиннинга из `name@version`).

type Entry struct {
	Source      string `yaml:"source"`      // git-URL (или локальный путь) репозитория
	Path        string `yaml:"path"`        // подпуть внутри репо (плагин — каталог, пресет — файл); дефолт "."
	Version     string `yaml:"version"`     // тег или ветка; дефолт "main"
	Commit      string `yaml:"commit"`      // v0.28a: полный SHA пина (теги переставляемы — SHA нет)
	Description string `yaml:"description"` // опционально
}

type Registry struct {
	Version string           `yaml:"version"` // версия формата реестра, "0.1"
	Plugins map[string]Entry `yaml:"plugins"`
	Presets map[string]Entry `yaml:"presets"`
}

const (
	FormatVersion      = "0.1"
	RegistryFile       = "registry.yaml"
	AddonsRegistryFile = "registry-addons.yaml"
	DefaultRegistryURL = "https://github.com/wykserdex/fidespipe"
)

// DefaultSource — локальный registry.yaml (или registry-addons.yaml) в CWD,
// иначе дефолтный URL-реестр.
func DefaultSource() string {
	if _, err := os.Stat(RegistryFile); err == nil {
		return RegistryFile
	}
	if _, err := os.Stat(AddonsRegistryFile); err == nil {
		return AddonsRegistryFile
	}
	return DefaultRegistryURL
}

// Handle — загруженный реестр.
type Handle struct {
	Registry *Registry
	Dir      string // каталог с registry.yaml (для локального — сам источник)
	tmp      string // временный клон (если источник был URL)
}

func (h *Handle) Close() error {
	if h.tmp != "" {
		return os.RemoveAll(h.tmp)
	}
	return nil
}

func (h *Handle) GetPlugin(name string) (Entry, bool) {
	e, ok := h.Registry.Plugins[name]
	return e, ok
}

func (h *Handle) GetPreset(name string) (Entry, bool) {
	e, ok := h.Registry.Presets[name]
	return e, ok
}

func (h *Handle) PluginNames() []string {
	out := make([]string, 0, len(h.Registry.Plugins))
	for n := range h.Registry.Plugins {
		out = append(out, n)
	}
	return out
}

func (h *Handle) PresetNames() []string {
	out := make([]string, 0, len(h.Registry.Presets))
	for n := range h.Registry.Presets {
		out = append(out, n)
	}
	return out
}

// Load — источник: локальный каталог с registry.yaml или git-URL (клон depth-1).
func Load(source string) (*Handle, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		source = DefaultSource()
	}

	// локальный каталог
	if fi, err := os.Stat(source); err == nil && fi.IsDir() {
		raw, err := os.ReadFile(filepath.Join(source, RegistryFile))
		if err != nil {
			return nil, fmt.Errorf("реестр %s: %w", source, err)
		}
		reg, err := parseRegistry(raw)
		if err != nil {
			return nil, err
		}
		return &Handle{Registry: reg, Dir: source}, nil
	}

	// локальный файл (registry.yaml в CWD или относительный путь)
	if fi, err := os.Stat(source); err == nil && fi.Mode().IsRegular() {
		raw, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		reg, err := parseRegistry(raw)
		if err != nil {
			return nil, err
		}
		return &Handle{Registry: reg, Dir: filepath.Dir(source)}, nil
	}

	// git-URL (или путь к bare-репо).
	// Источник валидируется ДО exec: ref вида --upload-pack=... иначе станет
	// опцией git (и выполнит произвольную команду от нашего имени). Аргумент-
	// разделитель -- закрывает класс инъекции для всего, что валидатор пропустит;
	// на Windows-пути и URL он безвреден (exec не проходит через shell).
	ref := source
	if strings.HasSuffix(source, "/") {
		ref = strings.TrimSuffix(source, "/")
	}
	if err := ValidateSource(ref); err != nil {
		return nil, fmt.Errorf("источник реестра: %w", err)
	}
	tmp, err := os.MkdirTemp("", "fidespipe-registry-*")
	if err != nil {
		return nil, err
	}
	out, err := runGit("clone", "--depth", "1", "--quiet", "--", ref, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("клон реестра %s: %s: %s", source, err, string(out))
	}
	raw, err := os.ReadFile(filepath.Join(tmp, RegistryFile))
	if err != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("в реестре %s нет %s", source, RegistryFile)
	}
	reg, err := parseRegistry(raw)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	return &Handle{Registry: reg, Dir: tmp, tmp: tmp}, nil
}

func parseRegistry(raw []byte) (*Registry, error) {
	var reg Registry
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&reg); err != nil {
		return nil, fmt.Errorf("registry.yaml: некорректный YAML: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("registry.yaml: несколько YAML-документов")
		}
		return nil, fmt.Errorf("registry.yaml: некорректный YAML: %w", err)
	}
	if reg.Version != FormatVersion {
		return nil, fmt.Errorf("registry.yaml: version %q, ожидается %q", reg.Version, FormatVersion)
	}
	reg.Plugins = normalizeEntries(reg.Plugins)
	reg.Presets = normalizeEntries(reg.Presets)
	for name, entry := range reg.Plugins {
		if err := ValidateComponent(name); err != nil {
			return nil, fmt.Errorf("registry.yaml: плагин: %w", err)
		}
		if err := ValidateEntry(entry, false); err != nil {
			return nil, fmt.Errorf("registry.yaml: плагин %q: %w", name, err)
		}
	}
	for name, entry := range reg.Presets {
		if err := ValidateComponent(name); err != nil {
			return nil, fmt.Errorf("registry.yaml: пресет: %w", err)
		}
		if err := ValidateEntry(entry, false); err != nil {
			return nil, fmt.Errorf("registry.yaml: пресет %q: %w", name, err)
		}
	}
	return &reg, nil
}

func safeEntryPath(path string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(path), `\`, "/")
	if normalized == "" || strings.HasPrefix(normalized, "/") || (len(normalized) >= 2 && normalized[1] == ':') {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(normalized))
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

var componentNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func ValidateEntry(entry Entry, requirePin bool) error {
	if !safeEntryPath(entry.Path) {
		return fmt.Errorf("небезопасный path %q", entry.Path)
	}
	if err := ValidateSource(entry.Source); err != nil {
		return err
	}
	if err := validateSourceVersion(entry.Version); err != nil {
		return err
	}
	if err := ValidateCommit(entry.Commit); err != nil {
		return err
	}
	if requirePin && isRemoteSource(entry.Source) && entry.Commit == "" {
		return fmt.Errorf("удалённый source %q требует полный commit pin", entry.Source)
	}
	return nil
}

func ValidateSource(source string) error {
	source = strings.TrimSpace(source)
	if source == "" || source != strings.TrimSpace(source) || strings.ContainsAny(source, "\x00\r\n\t ") || strings.HasPrefix(source, "-") || strings.Contains(source, "::") {
		return fmt.Errorf("небезопасный source %q", source)
	}
	if len(source) >= 2 && source[1] == ':' {
		return nil
	}
	parsed, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("некорректный source %q: %w", source, err)
	}
	if parsed.Scheme == "" {
		if strings.HasPrefix(source, "git@") && !strings.Contains(source, ":") {
			return fmt.Errorf("некорректный git source %q", source)
		}
		return nil
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		if parsed.Host == "" {
			return fmt.Errorf("source %q: отсутствует host", source)
		}
	case "ssh", "git":
		if parsed.Host == "" {
			return fmt.Errorf("source %q: отсутствует host", source)
		}
	case "file":
		if parsed.Path == "" {
			return fmt.Errorf("source %q: отсутствует путь", source)
		}
	default:
		return fmt.Errorf("source %q: неподдерживаемая схема %q", source, parsed.Scheme)
	}
	return nil
}

func validateSourceVersion(version string) error {
	version = strings.TrimSpace(version)
	if version == "" || version != strings.TrimSpace(version) || strings.ContainsAny(version, "\x00\r\n\t ") || strings.HasPrefix(version, "-") || strings.Contains(version, "..") || strings.Contains(version, "@{") || strings.Contains(version, "\\") || strings.Contains(version, "//") {
		return fmt.Errorf("небезопасный version %q", version)
	}
	return nil
}

func isRemoteSource(source string) bool {
	lower := strings.ToLower(strings.TrimSpace(source))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "ssh://") || strings.HasPrefix(lower, "git://") || strings.HasPrefix(lower, "file://") || strings.HasPrefix(lower, "git@")
}

func ValidateComponent(name string) error {
	if name == "" || strings.TrimSpace(name) != name || name == "." || name == ".." || strings.ContainsAny(name, `/\:`) || strings.ContainsRune(name, 0) || filepath.IsAbs(name) || filepath.Clean(name) != name || !componentNamePattern.MatchString(name) {
		return fmt.Errorf("небезопасное имя компонента %q", name)
	}
	return nil
}

func normalizeEntries(in map[string]Entry) map[string]Entry {
	if in == nil {
		in = map[string]Entry{}
	}
	for name, e := range in {
		if e.Path == "" {
			e.Path = "."
		} else {
			e.Path = strings.ReplaceAll(e.Path, `\`, "/")
		}
		if e.Version == "" {
			e.Version = "main"
		}
		in[name] = e
	}
	return in
}

// IsLocalRef — локальный путь: содержит разделитель пути, начинается с "." или "/".
// Голое имя (и имя@версия) — это имя из реестра.
// v0.29: Windows-абсолюты и Windows-разделители тоже считаются локальными.
func IsLocalRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	if common.IsBuiltinNamespace(ref) {
		return true
	}
	if strings.HasPrefix(ref, ".") || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, `\`) {
		return true
	}
	if filepath.IsAbs(ref) {
		return true
	}
	if len(ref) >= 2 && ref[1] == ':' {
		return true
	}
	return strings.ContainsAny(ref, `/\`)
}

// SplitRef — "name" или "name@version" → (name, version).
func SplitRef(ref string) (name, version string) {
	if i := strings.LastIndex(ref, "@"); i > 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

func NormalizePluginRef(ref string) (name, version string, ok bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(ref), `\`, "/")
	for _, prefix := range []string{"plugins/official/", "plugins/community/"} {
		if !strings.HasPrefix(normalized, prefix) {
			continue
		}
		rest := strings.TrimPrefix(normalized, prefix)
		if rest == "" || strings.Contains(rest, "/") {
			return "", "", false
		}
		name, version = SplitRef(rest)
		return name, version, true
	}
	return "", "", false
}

// RefToDir — резолвит ссылку `plugin:` в локальный каталог плагина.
// Локальные пути возвращаются как есть. Реестровые имена ищутся в
// <pluginsDir>/<name>, затем в <pluginsDir>/official/<name> и
// <pluginsDir>/community/<name>; flat-layout имеет приоритет.
// Запрошенная версия (@version) сверяется с lock-файлом .fidespipe.
func RefToDir(ref, pluginsDir string) (string, error) {
	if common.IsBuiltinNamespace(ref) {
		return "", fmt.Errorf("неизвестный встроенный модуль: %s", ref)
	}
	if IsLocalRef(ref) {
		return ref, nil
	}
	name, ver := SplitRef(ref)
	candidates := []string{
		filepath.Join(pluginsDir, name),
		filepath.Join(pluginsDir, "official", name),
		filepath.Join(pluginsDir, "community", name),
	}
	dir := ""
	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(candidate, "plugin.yaml")); err == nil {
			dir = candidate
			break
		}
	}
	if dir == "" {
		return "", fmt.Errorf("плагин %q не установлен: orchestrator plugin install %s", ref, ref)
	}
	if ver != "" {
		installed, ok := InstalledVersion(dir)
		if !ok {
			return "", fmt.Errorf("плагин %q: нет lock-файла .fidespipe, версия неизвестна — перустанови: orchestrator plugin install %s", name, ref)
		}
		if installed != ver {
			return "", fmt.Errorf("плагин %q: установлена версия %s, пайплайн требует %s — orchestrator plugin install %s", name, installed, ver, ref)
		}
	}
	return dir, nil
}
