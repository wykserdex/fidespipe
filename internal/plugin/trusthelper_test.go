package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// Помощники доверия для тестов.
//
// Тесты, которые проверяют НЕ доверие (лимит вывода, kill группы процессов,
// секреты, exit-коды), запускают плагины из t.TempDir(). После инверсии такой
// плагин внешний код по построению: его нет ни во встроенном allow-list, ни в
// конфиге оператора. Раньше те же тесты проходили потому, что доверенным
// считался любой плагин без строки `sandbox: untrusted` — то есть тестировали
// ровно то поведение, которое здесь и сломано.
//
// Помощник делает доверие ЯВНЫМ: allow-list собирается из реального хэша
// каталога плагина. Это не «доверять всему подряд» — запись по-прежнему
// проверяется по хэшу, и подмена файла после выдачи доверия её отзовёт.

// trustCtx — ctx с политикой, доверяющей перечисленным манифестам.
func trustCtx(t *testing.T, manifests ...*Manifest) context.Context {
	t.Helper()
	list := NewAllowList()
	for _, m := range manifests {
		digest, err := ContentDigest(m.Dir)
		if err != nil {
			t.Fatalf("хеш содержимого %s: %v", m.ID, err)
		}
		list.Allow(m.ID, digest)
	}
	return WithTrustPolicy(context.Background(), TrustPolicy{Trusted: list})
}

// trustAllCtx — то же, но allow-list строится по всему каталогу. Нужно там, где
// манифесты не собраны в список (например, каталог фикстур целиком).
// writePlainPlugin — временный плагин БЕЗ строки `sandbox`.
//
// Отдельный помощник, а не переиспользование writePlugin: тот намеренно ставит
// `sandbox: untrusted`, потому что проверяет песочницу. Здесь нужно наоборот —
// плагин, который ядро считает доверенным, чтобы тестировать поведение процесса
// (лимит вывода, потомки, секреты), а не отказ по политике.
func writePlainPlugin(t *testing.T, id, script string) (*pipeline.Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(script), 0o600); err != nil {
		t.Fatalf("write plugin: %v", err)
	}
	return &pipeline.Manifest{
		ID:      id,
		Version: "0.1.0",
		Runtime: pipeline.Runtime{Type: "python", Entry: "main.py"},
		Dir:     dir,
	}, dir
}
