//go:build js && wasm

package cli

import "os/exec"

func isWindows() bool { return false }

// В js/wasm нет ни групп процессов, ни сигналов: syscall.SysProcAttr пустой,
// а os/exec не запускает потомков вовсе. census — единственная команда,
// которой это нужно, и в браузере она не вызывается; заглушки здесь только
// ради того, чтобы дерево собиралось под wasm целиком.
func applyProcAttr(cmd *exec.Cmd) {}

func killTree(pid int) {}
