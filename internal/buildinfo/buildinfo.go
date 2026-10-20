// Package buildinfo — единственный источник версии продукта.
//
// Раньше версию знали двое: сборка (через ldflags в `-X …/api.Version`) и файл
// `VERSION` в ТЕКУЩЕМ каталоге, который читался в пяти местах и всегда
// перетирал инжектированное значение. Результат был обратен замыслу: релизный
// бинарник, запущенный из каталога с чужим `VERSION`, показывал чужую версию,
// а ldflags в релизном workflow были декоративны.
//
// Порядок разрешения теперь один и тот же для CLI, GUI и MCP:
//
//  1. версия, вшитая сборкой (`-X …/buildinfo.Version=0.34.0`) — побеждает всегда;
//  2. файл `VERSION` рядом с исполняемым файлом (dev-сборка из чекаута репо);
//  3. файл `VERSION` в текущем каталоге (dev-сборка через `go run`);
//  4. `dev`.
//
// Шаги 2–3 существуют только для сборок БЕЗ инжекта: релиз до них не доходит,
// поэтому содержимое рабочего каталога больше не может подменить версию релиза.
package buildinfo

import (
	"os"
	"path/filepath"
	"strings"
)

// Version вшивается сборкой:
//
//	go build -ldflags "-X github.com/wykserdex/fidespipe/internal/buildinfo.Version=0.33c"
//
// Пустое значение — признак dev-сборки, а не версия «пусто».
var Version = ""

// devVersion — то, что печатает сборка без инжекта и без файла VERSION.
const devVersion = "dev"

// maxVersionLen — защита от чтения не того файла: версия продукта коротка,
// а рядом с бинарником может лежать что угодно с именем VERSION.
const maxVersionLen = 64

// Resolve возвращает версию продукта по порядку из комментария пакета.
func Resolve() string {
	if v := strings.TrimSpace(Version); v != "" && v != devVersion {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		if v, ok := readVersionFile(filepath.Join(filepath.Dir(exe), "VERSION")); ok {
			return v
		}
	}
	if v, ok := readVersionFile("VERSION"); ok {
		return v
	}
	return devVersion
}

// readVersionFile читает версию из файла и отбрасывает содержимое, не похожее
// на версию. Файл с мусором — это не «нет файла»: вызывающий получает ok=false
// и идёт дальше по порядку, а не показывает мусор как свою версию.
func readVersionFile(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(raw))
	if v == "" || len(v) > maxVersionLen {
		return "", false
	}
	if strings.ContainsAny(v, "\n\r\t ") {
		return "", false
	}
	return v, true
}
