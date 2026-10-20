package execution

// Доверие фикстурным плагинам в тестах раннера.
//
// Тесты пишут плагин в t.TempDir() и запускают его через Run. После инверсии
// доверия (H1) такой плагин — внешний код: доверенным может быть только тот,
// чьё содержимое перечислено в allow-list ядра. Без явной выдачи доверия ран
// падал бы на отказе по политике, и тесты проверяли бы не то (например,
// TestCancelSleeper ждал cancelled от работающего плагина, а получал мгновенный
// fail).
//
// Помощник собирает allow-list из РЕАЛЬНОГО хэша каталога: доверие выдаётся
// явно и по содержимому, ровно как это делает оператор в fidespipe-trust.yaml.

import (
	"testing"

	"github.com/wykserdex/fidespipe/internal/plugin"
)

// trustPluginsDir — allow-list по каталогу с тестовыми плагинами.
func trustPluginsDir(t *testing.T, dir string) *plugin.AllowList {
	t.Helper()
	list, err := plugin.AllowListFromDirs(dir)
	if err != nil {
		t.Fatalf("allow-list фикстур (%s): %v", dir, err)
	}
	return list
}
