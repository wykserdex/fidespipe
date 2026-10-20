package guidirs

import (
	"fmt"
	"os"
	"strings"

	"github.com/wykserdex/fidespipe/internal/journal"
)

type Dirs struct {
	Plugins   string
	Pipelines string
	Runs      string
}

// Default — каталоги по умолчанию. Runs вычисляется тем же правилом, что и в
// CLI: каталог, который создаст ран, должен быть тем же, из которого GUI и
// лаунчер его прочитают.
func Default() Dirs {
	return Dirs{Plugins: "plugins", Pipelines: "examples", Runs: journal.DefaultRunsDir()}
}

func (d Dirs) Ensure() error {
	for _, item := range []struct {
		name string
		path string
	}{
		{name: "plugins", path: d.Plugins},
		{name: "pipelines", path: d.Pipelines},
		{name: "runs", path: d.Runs},
	} {
		if strings.TrimSpace(item.path) == "" {
			return fmt.Errorf("%s: каталог не указан", item.name)
		}
		if err := os.MkdirAll(item.path, 0755); err != nil {
			return fmt.Errorf("%s: %w", item.name, err)
		}
	}
	return nil
}

func Parse(args []string, defaults Dirs) (Dirs, []string, error) {
	dirs := defaults
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		var name, value string
		var hasValue bool
		switch {
		case strings.HasPrefix(a, "--plugins="):
			name, value, hasValue = "--plugins", strings.TrimPrefix(a, "--plugins="), true
		case strings.HasPrefix(a, "--pipelines="):
			name, value, hasValue = "--pipelines", strings.TrimPrefix(a, "--pipelines="), true
		case strings.HasPrefix(a, "--runs-dir="):
			name, value, hasValue = "--runs-dir", strings.TrimPrefix(a, "--runs-dir="), true
		case a == "--plugins" || a == "--pipelines" || a == "--runs-dir":
			name = a
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return Dirs{}, nil, fmt.Errorf("%s: нужно значение", name)
			}
			i++
			value, hasValue = args[i], true
		default:
			rest = append(rest, a)
			continue
		}
		if !hasValue || strings.TrimSpace(value) == "" {
			return Dirs{}, nil, fmt.Errorf("%s: нужно значение", name)
		}
		switch name {
		case "--plugins":
			dirs.Plugins = value
		case "--pipelines":
			dirs.Pipelines = value
		case "--runs-dir":
			dirs.Runs = value
		}
	}
	return dirs, rest, nil
}
