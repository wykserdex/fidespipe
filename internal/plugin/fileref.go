package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

type FileRefManifest = pipeline.Manifest

func FileRefWarnings(m *pipeline.Manifest, input map[string]interface{}) []string {
	root, _ := os.Getwd()
	var warns []string
	names := make([]string, 0, len(m.Input))
	for name := range m.Input {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		port := m.Input[name]
		if port.Format != "file_ref" {
			continue
		}
		v, ok := input[name].(string)
		if !ok || v == "" || filepath.IsAbs(v) {
			continue
		}
		pluginAbs, err := filepath.Abs(m.Dir)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(pluginAbs, v)); err == nil {
			continue
		}
		msg := fmt.Sprintf(
			"вход %s = %q — относительный путь не найден от рабочей директории плагина (%s): subprocess плагина стартует в его собственной директории",
			name, v, pluginAbs)
		if root != "" {
			if _, err := os.Stat(filepath.Join(root, v)); err == nil {
				msg += "; такой путь есть от КОРНЯ ПРОЕКТА — вероятно, вы имели в виду его: укажите путь абсолютно или относительно директории плагина"
			}
		}
		warns = append(warns, msg)
	}
	return warns
}
