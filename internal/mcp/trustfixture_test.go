package mcp

// Доверие фикстурным плагинам в тестах MCP.
//
// После инверсии доверия (H1) доверенность решает ядро по хэшу содержимого, и
// плагин из t.TempDir() внешним кодом по построению. Тесты, которые проверяют
// прочие свойства (журнал аудита, запуск, отмена), должны доверять своей
// фикстуре явно — иначе они проверяли бы отказ по политике вместо заявленного.

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

// trustManifest — внести каталог плагина в allow-list и вернуть манифест.
//
// Манифест читается напрямую с диска, а не через s.multi: часть тестов пишет
// аудит, минуя multiEngine, и обращение к nil-полю упало бы с пангой вместо
// внятной ошибки. Имя плагина берётся из plugin.yaml — тем же путём, каким
// его увидит ядро.
func trustManifest(t *testing.T, s *Server, dir string) *pipeline.Manifest {
	t.Helper()
	m := &pipeline.Manifest{ID: pluginIDFromDir(t, dir), Dir: dir}
	digest, err := plugin.ContentDigest(dir)
	if err != nil {
		t.Fatalf("хеш содержимого %s: %v", m.ID, err)
	}
	if s.trust.Trusted == nil {
		s.trust.Trusted = plugin.NewAllowList()
	}
	s.trust.Trusted.Allow(m.ID, digest)
	return m
}

// pluginIDFromDir — id из plugin.yaml каталога (имя каталога — запасной вариант).
func pluginIDFromDir(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		t.Fatalf("plugin.yaml в %s: %v", dir, err)
	}
	var m struct {
		ID string `yaml:"id"`
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("plugin.yaml в %s: %v", dir, err)
	}
	if m.ID == "" {
		return filepath.Base(dir)
	}
	return m.ID
}

// trustDirsIn — allow-list по корню с плагинами (имя плагина = имя каталога).
func trustDirsIn(t *testing.T, root string) *plugin.AllowList {
	t.Helper()
	list, err := plugin.AllowListFromDirs(root)
	if err != nil {
		t.Fatalf("allow-list фикстур (%s): %v", root, err)
	}
	return list
}

// echoerScript — минимальный честный плагин: съел stdin, ответил конвертом.
const echoerScript = "import sys\nsys.stdin.read()\nsys.stdout.write('{\"status\":\"ok\",\"output\":{}}')\n"

// writeFakePluginDir — каталог плагина с манифестом и скриптом.
func writeFakePluginDir(t *testing.T, root, id, script string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "id: " + id + "\nversion: 0.1.0\nplatform_api: \"0.1\"\n" +
		"runtime:\n  type: python\n  entry: main.py\ninput: {}\noutput: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
