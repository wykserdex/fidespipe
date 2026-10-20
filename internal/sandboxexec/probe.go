//lint:file-ignore SA4023 Проверка «сравнение всегда истинно» срабатывает здесь
// только на платформе без изолятора: appcontainer.Run на ней отказывает всегда
// (заглушка возвращает ErrUnsupported), и `if runErr != nil` выглядит
// тождественно истинным. В сборке с изолятором (Windows) nil-ошибка — штатный
// путь, и проверка обязательна. Подавлено на уровне файла, а не строки:
// директива //lint:ignore обязана что-то подавлять и на Windows ругается
// «didn't match anything», то есть роняет CI на зелёном коде.

package sandboxexec

// Проба хоста: умеет ли capability internetClient дать контейнеру сеть.
//
// Проверка не «соединился ли плагин» — это нельзя отличить от поломанного
// плагина. Проверка «может ли контейнер вообще создать сокет»: без сети
// внутри контейнера не инициализируется Winsock.
//
// Проба нужна для fail-closed: capability может быть не зарегистрирована в
// системе (нет таблицы capability), и тогда сеть не появится, хотя
// capability в токен добавлена. Молча запускать плагин без сети нельзя —
// он будет падать с ошибкой, которую примет за свою собственную.
//
// Живёт здесь, а не в appcontainer, потому что знает имя скрытого режима
// самопроверки: appcontainer о таком режиме ничего не знает и знать не должен.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wykserdex/fidespipe/internal/appcontainer"
)

// ProbeNetwork — запускает самопроверку в контейнере с capability.
//
// helper — путь к исполняемому файлу, умеющему запускать самопроверку
// (это тот же бинарь Fidespipe). Возвращает: работает ли сеть, что именно
// ответила проба, и ошибку запуска, если она была.
func ProbeNetwork(ctx context.Context, helper string) (bool, string, error) {
	if helper == "" {
		return false, "", fmt.Errorf("не указан помощник для пробы сети")
	}
	scratch, err := os.MkdirTemp("", "fidespipe-netprobe-")
	if err != nil {
		return false, "", fmt.Errorf("каталог для пробы сети: %w", err)
	}
	defer os.RemoveAll(scratch)
	result := filepath.Join(scratch, "result.txt")

	_, runErr := appcontainer.Run(ctx, appcontainer.Plan{
		// Имя профиля без префикса "fidespipe." — ProfileName его уже добавляет,
		// и с лишним получалось "fidespipe.fidespipe.netprobe" в журнале AppModel.
		Name:         appcontainer.ProfileName("netprobe"),
		Argv:         []string{helper, SelfCommandName, result},
		Read:         appcontainer.Readable{filepath.Dir(helper)},
		Scratch:      scratch,
		WorkDir:      scratch,
		Env:          []string{"PATH=" + os.Getenv("PATH")},
		AllowNetwork: true,
	})
	if runErr != nil {
		return false, "", runErr
	}
	data, readErr := os.ReadFile(result)
	if readErr != nil {
		return false, "", fmt.Errorf("проба сети не дала результата: %w", readErr)
	}
	text := strings.TrimSpace(string(data))
	return strings.HasPrefix(text, "NET_OK"), text, nil
}
