package core

import (
	"os"
	"path/filepath"

	"github.com/wykserdex/fidespipe/internal/plugin"
)

// ScanPlugins — плагины из plugins/official, plugins/community и plugins
// (v0.17: общий сканер для cmd/orchestrator и cmd/tool; CI проверяет и тот, и другой).
func ScanPlugins() []Manifest {
	eng := NewEngine()
	roots := []string{"plugins", "plugins/official", "plugins/community", "plugins/" + plugin.AgentPluginDir}
	var out []Manifest
	seen := map[string]bool{}
	for _, root := range roots {
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, e.Name())
			m, err := eng.LoadManifest(dir)
			if err != nil || m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, *m)
		}
	}
	return out
}
