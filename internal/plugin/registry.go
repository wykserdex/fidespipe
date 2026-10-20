package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wykserdex/fidespipe/internal/common"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/registry"
)

type Engine struct {
	mu         sync.Mutex
	Cache      map[string]*pipeline.Manifest
	PluginsDir string // дефолт "plugins"
}

func NewEngine() *Engine {
	return &Engine{
		Cache:      make(map[string]*pipeline.Manifest),
		PluginsDir: "plugins",
	}
}

func IsBuiltin(ref string) bool {
	return pipeline.IsBuiltin(ref)
}

func IsBuiltinNamespace(ref string) bool {
	return pipeline.IsBuiltinNamespace(ref)
}

// builtinManifests — контракты встроенных модулей. Раньше здесь возвращался
// один захардкоженный манифест human_gate на ЛЮБОЙ ref из namespace core/,
// потому что встроенным был ровно один модуль. С появлением core/text_stats
// манифест выбирается по ref: иначе шаг с text_stats получил бы контракт гейта
// (у него нет ни input, ни output) и его выход просто не проверялся бы.
func builtinManifests() map[string]*pipeline.Manifest {
	return map[string]*pipeline.Manifest{
		common.HumanGatePluginRef: {
			ID:          common.HumanGatePluginRef,
			Version:     pipeline.PlatformAPI,
			PlatformAPI: pipeline.PlatformAPI,
			// У гейта нет ни входов, ни выходов: он не читает данные и его
			// результат — решение человека, а не значение для steps.*.
		},
		common.TextStatsPluginRef: {
			ID:          common.TextStatsPluginRef,
			Version:     pipeline.PlatformAPI,
			PlatformAPI: pipeline.PlatformAPI,
			Description: "Метрики текста: строки, слова, уникальные слова, длиннейшее слово. Встроенный, не требует Python",
			Input: map[string]pipeline.Port{
				"text": {From: "input.text", Type: "string", Format: "text"},
			},
			Output: map[string]pipeline.Port{
				"lines":        {Type: "number"},
				"words":        {Type: "number"},
				"unique_words": {Type: "number"},
				"longest_word": {Type: "string"},
			},
			// Порт встроенного модуля по построению не трогает ни сеть, ни
			// файлы, ни секреты — объявлять тут нечего, и declare-now-проверки
			// сети на нём не срабатывают.
			Permissions: pipeline.Permissions{Filesystem: "none"},
		},
	}
}

func (e *Engine) LoadManifest(ref string) (*pipeline.Manifest, error) {
	if IsBuiltin(ref) {
		canonical, _ := common.CanonicalBuiltinRef(ref)
		if m, ok := builtinManifests()[canonical]; ok {
			return m, nil
		}
		return nil, fmt.Errorf("встроенный модуль без манифеста: %s", ref)
	}
	if IsBuiltinNamespace(ref) {
		return nil, fmt.Errorf("неизвестный встроенный модуль: %s", ref)
	}
	e.mu.Lock()
	if e.Cache == nil {
		e.Cache = make(map[string]*pipeline.Manifest)
	}
	if m, ok := e.Cache[ref]; ok {
		e.mu.Unlock()
		return m, nil
	}
	e.mu.Unlock()
	// v0.16: голое имя (или имя@версия) — реестр; пути — как раньше.
	dir, err := registry.RefToDir(ref, e.PluginsDir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return nil, fmt.Errorf("плагин %q: не читается plugin.yaml: %w", ref, err)
	}
	var m pipeline.Manifest
	if err := pipeline.DecodeManifest(raw, &m); err != nil {
		return nil, fmt.Errorf("плагин %q: некорректный манифест: %w", ref, err)
	}
	m.Dir = dir
	if err := pipeline.ValidateManifest(&m); err != nil {
		return nil, fmt.Errorf("плагин %q: некорректный манифест: %w", ref, err)
	}
	e.mu.Lock()
	e.Cache[ref] = &m
	e.mu.Unlock()
	return &m, nil
}

// donorEnvFor — переменная окружения с путём к донору плагина.
//
// Путь берётся из метки установки (.fidespipe): её пишет установщик, когда ставит
// донора. Читать её в рантайме — единственный способ узнать, куда установщик
// положил файл: путь зависит от корня установки, а он у ядра другой.
//
// Пусто, если донора нет, он не ставился или плагин объявил переменную с
// недопустимым именем (тогда подмена переменных окружения была бы уязвимостью).
func donorEnvFor(m *pipeline.Manifest) string {
	if m == nil || m.Runtime.Donor == nil {
		return ""
	}
	name := strings.TrimSpace(m.Runtime.Donor.BinEnv)
	if name == "" || !validEnvName(name) {
		return ""
	}
	if m.Dir == "" {
		return ""
	}
	lock, err := registry.ReadLock(m.Dir)
	if err != nil || strings.TrimSpace(lock.DonorPath) == "" {
		return ""
	}
	return name + "=" + lock.DonorPath
}
