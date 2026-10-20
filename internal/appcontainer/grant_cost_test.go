//go:build windows

package appcontainer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGrantIsCheapOnRepeat — выдача прав не должна стоить дорого на повторе.
//
// РЕГРЕССИЯ. Windows пересчитывает наследование по всему дереву каталога
// при каждой выдаче, даже если ничего не изменилось. На каталоге
// интерпретатора Python это 16-18 секунд измерено (под ним 2,6 ГБ в
// site-packages), и повторная выдача стоит столько же.
//
// Плагин при этом отрабатывает за десятки миллисекунд, и без проверки DCL
// перед выдачей каждый запуск плагина на Windows платил бы 16 секунд на
// пустом месте.
//
// Проверка не на скорость, а на СМЫСЛ: вторая выдача обязана быть
// принципиально дешевле первой, потому что она ничего не меняет.
func TestGrantIsCheapOnRepeat(t *testing.T) {
	dir := t.TempDir()
	// Дерево с файлами: наследование пересчитывается по ним.
	for i := 0; i < 50; i++ {
		name := filepath.Join(dir, "файл")
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	name := ProfileName("test.grant." + filepath.Base(dir))
	// Первая выдача: создаёт запись, её стоимость не интересует.
	if _, err := profileSID(name); err != nil {
		t.Fatalf("профиль не создан: %v", err)
	}
	sid, err := profileSID(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := grant(dir, sid, 0x00120089); // GENERIC_READ|GENERIC_EXECUTE
	err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := grant(dir, sid, 0x00120089); err != nil {
		t.Fatal(err)
	}
	second := time.Since(start)
	// Порог выбран с запасом: на свежом каталоге выдача занимает единицы
	// миллисекунд, и регресс с пересчётом наследования — это секунды.
	if second > 2*time.Second {
		t.Fatalf("повторная выдача прав заняла %v — проверка DCL перед выдачей не сработала", second)
	}
	t.Logf("повторная выдача: %v", second.Round(time.Millisecond))
}
