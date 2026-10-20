//go:build windows

package appcontainer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestCapabilityInProfile — capability, выданная ПРОФИЛЮ, а не процессу.
//
// Проверяется измерением, потому что мнений тут два: одни считают, что
// capability надо выдавать профилю при создании, другие — процессу при
// запуске. Проводка сделана и там, и там; теперь видно, что работает.
//
// Проба: создаётся сокет. Соединение не нужно — без сети внутри контейнера
// не инициализируется Winsock, и падает именно создание сокета.
func TestCapabilityInProfile(t *testing.T) {
	scratch := t.TempDir()
	probe, cleanup := buildSocketProbe(t)
	defer cleanup()

	capSID, err := internetClientSID()
	if err != nil {
		t.Fatal(err)
	}
	caps := &windows.SIDAndAttributes{Sid: capSID, Attributes: windows.SE_GROUP_ENABLED}

	// Профиль с capability, процесс без capability.
	profName := ProfileName("test.profcap")
	sid, err := profileSIDCaps(profName, caps)
	if err != nil {
		t.Fatalf("профиль с capability не создан: %v", err)
	}
	t.Logf("профиль создан, SID=%s", sid.String())

	out := filepath.Join(scratch, "result.txt")
	_, err = Run(context.Background(), Plan{
		Name:    profName,
		Argv:    []string{probe, out},
		Read:    Readable{filepath.Dir(probe)},
		Scratch: scratch,
		WorkDir: scratch,
		Env:     []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatalf("запуск: %v", err)
	}
	data, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("результата нет: %v", rerr)
	}
	t.Logf("профиль с capability, процесс без: %s", data)
}
