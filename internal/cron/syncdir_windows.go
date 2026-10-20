//go:build windows

package cron

// syncDir на Windows — ничего.
//
// Каталог на Windows открыть и синхронизировать нельзя: fsync директории там
// не существует, а попытка даёт «Access is denied». Перенёс снимок и так
// переживает падение: NTFS журналирует операции метаданных, и после
// rename каталога состояние либо есть целиком, либо его нет вовсе.
func syncDir(dir string) error { return nil }
