//go:build !windows

package main

import "fmt"

// desktop (не Windows) — dev-путь: GUI в системном браузере, процесс живёт
// до Ctrl+C. Настоящее окно — на Windows (WebView2, desktop_windows.go).
func desktop(url string, debug bool, logf func(string, ...interface{})) {
	logf("окно WebView2 — только Windows; здесь GUI открыт в браузере")
	// ссылка с одноразовым кодом — в консоль (не в лог-файл): если браузер
	// не открылся, её можно открыть руками
	fmt.Println("  ссылка с кодом входа (только для вас):", url)
	openBrowser(url)
	waitSignal()
}
