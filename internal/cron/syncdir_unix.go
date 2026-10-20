//go:build !windows

package cron

import "os"

// syncDir — fsync каталога, чтобы переименование снимка пережило падение
// питания.
//
// На Unix это обязательный шаг: rename атомарен, но без fsync каталога после
// него переименование может не пережить перезагрузку — и очередь решит, что
// слот не отработал, и запустит его повторно.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
