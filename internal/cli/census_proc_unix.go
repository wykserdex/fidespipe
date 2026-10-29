//go:build !windows && !js

package cli

import (
	"os/exec"
	"syscall"
)

func isWindows() bool { return false }

// Своя группа процессов: иначе go test наследует процессную группу терминала,
// и Ctrl+C в консоли убивает не то.
func applyProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Убиваем всю группу: у `go test` есть дети (сам тест-бинарник), и убийство
// только родителя оставляет их держать открытые файлы и пайпы.
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
