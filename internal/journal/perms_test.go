package journal

// Права на каталоги и файлы рана. В них лежат входы шагов и их выходы, то есть
// содержимое данных пользователя; при 0755/0644 их читал любой локальный
// пользователь машины.
//
// На Windows режимы не действуют (права задаёт DACL каталога, см.
// SECURITY.md), поэтому там проверяется другое: вызовы обязаны проходить без
// ошибок. Кроссплатформенность — часть требования, а не оговорка.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wykserdex/fidespipe/internal/runctx"
)

// Контракт режимов проверяется на любой платформе, потому что на Windows
// режимы не действуют и проверка на диске была бы вакуумно-зелёной. Сами
// значения — 0700 на каталоге и 0600 на файле: 0755/0644 отдавали содержимое
// рана любому локальному пользователю машины.
func TestRunPermissionsArePrivate(t *testing.T) {
	if runDirPerm != 0o700 {
		t.Errorf("каталог рана создаётся с режимом %04o, ждали 0700", runDirPerm)
	}
	if filePerm != 0o600 {
		t.Errorf("файлы рана создаются с режимом %04o, ждали 0600", filePerm)
	}
	// Чужие права на этих режимах — тоже ошибка, а не «ну почти».
	if runDirPerm&0o077 != 0 || filePerm&0o077 != 0 {
		t.Errorf("режимы открыты группе и остальным: каталог %04o, файл %04o", runDirPerm, filePerm)
	}
}

// wantPerms — что ожидается на POSIX и что проверяется на Windows.
func wantPerms(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if runtime.GOOS == "windows" {
		// Права на Windows не несут смысла; важно лишь, что путь существует и
		// читается. Проверка режима тут была бы вакуумно-зелёной.
		if info.IsDir() {
			return
		}
		if _, err := os.ReadFile(path); err != nil {
			t.Fatalf("%s не читается: %v", path, err)
		}
		return
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s: режим %04o, ждали %04o — содержимое рана доступно другим пользователям машины",
			path, got, want)
	}
}

func TestRunDirectoryAndJournalArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "20260929-120000-demo")
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Event("run_start", map[string]interface{}{"pipeline": "demo"}); err != nil {
		t.Fatal(err)
	}
	ctx := runctx.NewCtx(map[string]interface{}{"input": "секретный вход"})
	if err := j.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	j.Close()

	wantPerms(t, dir, 0o700)
	wantPerms(t, filepath.Join(dir, "journal.jsonl"), 0o600)
	// context.json создаётся только как temp+rename, а rename сохраняет режим
	// источника, поэтому проверка итогового файла — это и проверка temp.
	// Отдельно temp не проверить: он либо переименовывается, либо удаляется
	// на неудаче (writeSnapshot), и в обоих случаях наружу не торчит.
	wantPerms(t, filepath.Join(dir, "context.json"), 0o600)
}

// Повторное открытие рана (resume) обязано вести себя так же.
func TestOpenAppendKeepsPrivateJournal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "20260929-120000-demo")
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()

	again, err := OpenJournalAppend(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if err := again.Event("run_end", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	wantPerms(t, filepath.Join(dir, "journal.jsonl"), 0o600)
}

func TestArtifactsAndIndexArePrivate(t *testing.T) {
	store := NewJsonStore(t.TempDir(), "")
	j, err := store.Create("20260929-120000-demo")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := store.SaveArtifact("20260929-120000-demo", "report.csv", []byte("a,b\n1,2\n")); err != nil {
		t.Fatal(err)
	}
	wantPerms(t, filepath.Join(store.BaseDir, "20260929-120000-demo", "artifacts"), 0o700)
	wantPerms(t, filepath.Join(store.BaseDir, "20260929-120000-demo", "artifacts", "report.csv"), 0o600)
	wantPerms(t, store.DBPath, 0o600)
}
