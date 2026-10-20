package guidirs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wykserdex/fidespipe/internal/journal"
)

func TestDefaultAndParse(t *testing.T) {
	defaults := Default()
	// Runs не константа, а следствие общего правила выбора каталога прогонов:
	// GUI обязан читать оттуда же, откуда пишет ран.
	if defaults.Plugins != "plugins" || defaults.Pipelines != "examples" {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}
	if defaults.Runs != journal.DefaultRunsDir() {
		t.Fatalf("Runs = %q, а правило выбора каталога даёт %q",
			defaults.Runs, journal.DefaultRunsDir())
	}
	got, rest, err := Parse([]string{
		"--plugins=custom/plugins",
		"--pipelines", "custom/pipelines",
		"--runs-dir=custom/runs",
		"--open",
	}, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plugins != "custom/plugins" || got.Pipelines != "custom/pipelines" || got.Runs != "custom/runs" {
		t.Fatalf("unexpected dirs: %+v", got)
	}
	if len(rest) != 1 || rest[0] != "--open" {
		t.Fatalf("unexpected rest: %v", rest)
	}
}

func TestParseMissingValue(t *testing.T) {
	if _, _, err := Parse([]string{"--pipelines"}, Default()); err == nil {
		t.Fatal("expected missing value error")
	}
}

func TestEnsure(t *testing.T) {
	root := t.TempDir()
	dirs := Dirs{
		Plugins:   filepath.Join(root, "plugins"),
		Pipelines: filepath.Join(root, "pipelines"),
		Runs:      filepath.Join(root, "runs"),
	}
	if err := dirs.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dirs.Plugins, dirs.Pipelines, dirs.Runs} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}
