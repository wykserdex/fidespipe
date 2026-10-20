//go:build !windows

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validTrustBody = "version: \"0.1\"\ntrusted_plugins:\n  - \"p@sha256:" +
	"0000000000000000000000000000000000000000000000000000000000000000\"\n"

// Целостность конфига доверия (аудит N3): файл, который мог изменить кто-то
// кроме владельца, не читается вовсе.
func TestLoadTrustConfigRejectsUnsafeFile(t *testing.T) {
	newFile := func(t *testing.T, mode os.FileMode) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "fidespipe-trust.yaml")
		if err := os.WriteFile(path, []byte(validTrustBody), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("0600 читается", func(t *testing.T) {
		list, err := LoadTrustConfig(newFile(t, 0o600))
		if err != nil || list.Len() != 1 {
			t.Fatalf("безопасный файл должен читаться: len=%d err=%v", list.Len(), err)
		}
	})
	t.Run("0644 читается", func(t *testing.T) {
		if _, err := LoadTrustConfig(newFile(t, 0o644)); err != nil {
			t.Fatalf("файл, записываемый только владельцем, должен читаться: %v", err)
		}
	})
	for _, mode := range []os.FileMode{0o664, 0o666, 0o620, 0o602} {
		mode := mode
		t.Run("запись для группы/всех "+mode.String(), func(t *testing.T) {
			_, err := LoadTrustConfig(newFile(t, mode))
			if err == nil || !strings.Contains(err.Error(), "небезопасен") {
				t.Fatalf("файл с правами %v не должен читаться, err=%v", mode, err)
			}
		})
	}
	t.Run("симлинк", func(t *testing.T) {
		real := newFile(t, 0o600)
		link := filepath.Join(t.TempDir(), "fidespipe-trust.yaml")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("симлинки недоступны: %v", err)
		}
		if _, err := LoadTrustConfig(link); err == nil || !strings.Contains(err.Error(), "симлинк") {
			t.Fatalf("симлинк должен отвергаться, err=%v", err)
		}
	})
	t.Run("каталог вместо файла", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "fidespipe-trust.yaml")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTrustConfig(dir); err == nil {
			t.Fatal("каталог не может быть конфигом доверия")
		}
	})
	t.Run("каталог доступен всем на запись", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o777); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "fidespipe-trust.yaml")
		if err := os.WriteFile(path, []byte(validTrustBody), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTrustConfig(path); err == nil || !strings.Contains(err.Error(), "каталог") {
			t.Fatalf("конфиг в каталоге, доступном всем на запись, должен отвергаться, err=%v", err)
		}
	})
	t.Run("отсутствие файла — не ошибка", func(t *testing.T) {
		list, err := LoadTrustConfig(filepath.Join(t.TempDir(), "нет.yaml"))
		if err != nil || list.Len() != 0 {
			t.Fatalf("len=%d err=%v", list.Len(), err)
		}
	})
}
