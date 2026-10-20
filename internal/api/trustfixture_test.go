package api

// Доверие фикстурным плагинам в тестах API.
//
// Тесты API поднимают плагины в t.TempDir(). После инверсии (H1) такой плагин —
// внешний код: его нет ни во встроенном allow-list, ни в fidespipe-trust.yaml.
// Без явной выдачи доверия ран падал бы на отказе по политике, и тест проверял
// бы не то, что думал (например, TestAPIRunCancel ждал отмены работающего
// плагина, а получал мгновенный fail).
//
// Помощник собирает allow-list из РЕАЛЬНОГО хэша каталога: доверие выдаётся
// явно и по содержимому, ровно так же, как это делает оператор.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

// trustDirIn — внести в allow-list сервера плагины из каталога dir
// (включая вложенные official/ и community/).
func trustDirIn(t *testing.T, srv *Server, dir string) {
	t.Helper()
	roots := []string{dir}
	for _, sub := range []string{"official", "community", plugin.AgentPluginDir} {
		roots = append(roots, filepath.Join(dir, sub))
	}
	found := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pluginDir := filepath.Join(root, e.Name())
			if _, err := os.Stat(filepath.Join(pluginDir, "plugin.yaml")); err != nil {
				continue
			}
			m, err := srv.Engine.LoadManifest(pluginDir)
			if err != nil {
				continue
			}
			trustManifestIn(t, srv, m)
			found++
		}
	}
	if found == 0 {
		t.Fatalf("trustDirIn: в %s не найдено ни одного плагина — доверие выдавать нечему", dir)
	}
}

// trustManifestIn — внести конкретный плагин (по его манифесту) в allow-list.
func trustManifestIn(t *testing.T, srv *Server, m *pipeline.Manifest) {
	t.Helper()
	digest, err := plugin.ContentDigest(m.Dir)
	if err != nil {
		t.Fatalf("хеш содержимого %s: %v", m.ID, err)
	}
	if srv.Trusted == nil {
		srv.Trusted = plugin.NewAllowList()
	}
	srv.Trusted.Allow(m.ID, digest)
}

// loadPluginForTrust — загрузить манифест плагина из каталога.
func loadPluginForTrust(t *testing.T, srv *Server, dir string) *pipeline.Manifest {
	t.Helper()
	m, err := srv.Engine.LoadManifest(dir)
	if err != nil {
		t.Fatalf("манифест %s: %v", dir, err)
	}
	return m
}

// trustPluginsDir — один каталог, который нужно внести в allow-list сервера.
// Именованная обёртка нужна, чтобы в вызове pluginsAPI(t, root, trustDir) не
// путать, что передаётся: каталог плагинов или каталоги для доверия.
func trustPluginsDir(dir string) string { return dir }
