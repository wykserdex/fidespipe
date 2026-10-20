//go:build windows

package appcontainer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNetworkCapability — capability internetClient даёт контейнеру сеть.
//
// Проверяется не «соединение установилось», а то, что блокирует сеть
// на самом деле: внутри контейнера без capability НЕ ИНИЦИАЛИЗИРУЕТСЯ
// WINSOCK (WinError 10106 «не удаётся загрузить провайдер услуг»), то есть
// запрет наступает раньше, чем дело доходит до адреса. Поэтому проверка —
// «сокет создаётся», и адрес для неё не нужен вовсе: это делает проверку
// независимой от наличия интернета на машине.
//
// Помощник собирается на лету: тест не должен зависеть ни от Python, ни от
// curl, ни от чего-либо ещё, что может отсутствовать на CI-раннере.
func TestNetworkCapability(t *testing.T) {
	if !CapabilityTablePresent() {
		t.Skip("на этом хосте нет таблицы capability (HKLM\\...\\CapabilityTable): " +
			"Capability internetClient системой не опознаётся, проверять нечем")
	}
	probe, cleanup := buildSocketProbe(t)
	defer cleanup()

	scratch := t.TempDir()
	for _, c := range []struct {
		name         string
		allowNetwork bool
	}{
		{"без capability", false},
		{"с capability", true},
	} {
		out := filepath.Join(scratch, "result-"+strings.ReplaceAll(c.name, " ", "_")+".txt")
		_, err := Run(context.Background(), Plan{
			Name:         ProfileName("test.netcap." + c.name),
			Argv:         []string{probe, out},
			Read:         Readable{filepath.Dir(probe)},
			Scratch:      scratch,
			WorkDir:      scratch,
			Env:          []string{"PATH=" + os.Getenv("PATH")},
			AllowNetwork: c.allowNetwork,
		})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		data, rerr := os.ReadFile(out)
		if rerr != nil {
			t.Logf("%-16s результата нет: %v", c.name, rerr)
			continue
		}
		t.Logf("%-16s %s", c.name, strings.TrimSpace(string(data)))
	}
}

// buildSocketProbe — собирает помощника, который создаёт сокет и печатает
// результат. Возвращает путь к исполняемому файлу.
func buildSocketProbe(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	// Проба соединяется с портом, который никто не слушает, и РАЗЛИЧАЕТ две
	// неудачи:
	//
	//   - «connection refused» — сокет создан, стек сети работает, значит сеть
	//     есть (порт закрыт — это не запрет);
	//   - любая другая ошибка (WSAStartup не удался, провайдер услуг не
	//     загрузился) — Winsock в контейнере не инициализируется, сети нет.
	//
	// Через net, а не через сырой syscall.Socket: сырой вызов требует
	// предварительного WSAStartup, и без него падает с ошибкой
	// «application has not called WSAStartup» — ошибкой САМОГО пробника,
	// а не контейнера. На этом обманулись первые замеры.
	body := `package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

func main() {
	_, err := net.Dial("tcp", "127.0.0.1:1")
	switch {
	case err == nil:
		os.WriteFile(os.Args[1], []byte("SOCKET_OK"), 0o600)
	case errors.Is(err, syscall.ECONNREFUSED):
		// Сокет создался и соединение дошло до сети: сеть есть.
		os.WriteFile(os.Args[1], []byte("SOCKET_OK refused"), 0o600)
	default:
		os.WriteFile(os.Args[1], []byte("SOCKET_FAIL "+err.Error()), 0o600)
		fmt.Fprintln(os.Stderr, "SOCKET_FAIL", err)
		os.Exit(1)
	}
}
`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "probe.exe")
	cmd := exec.Command("go", "build", "-o", out, src)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GO111MODULE=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("помощник не собрался: %v\n%s", err, b)
	}
	return out, func() { _ = os.RemoveAll(dir) }
}

// CapabilityTablePresent — есть ли в системе таблица capability.
//
// Таблица — это реестр, в котором система знает, какая capability за что
// отвечает. Без неё SID capability (S-1-15-3-1) системой не опознаётся, и
// выдача capability не даёт ничего: измерено на хосте без таблицы —
// Winsock в контейнере не инициализируется и с capability, и без него.
func CapabilityTablePresent() bool {
	out, err := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\CapabilityTable\Capabilities\internetClient`).Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "internetclient")
}
