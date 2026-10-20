package sandboxexec

// Режим самопроверки сети: создать сокет и сказать, получилось ли.
//
// Нужен для проверки capability: без неё ответ выглядел бы одинаково —
// «плагин не смог выйти в сеть» — и нечем было бы отличить запрет песочницы
// от поломанного плагина. Проба идёт ДО сокета, а не до адреса: без сети
// внутри контейнера не инициализируется Winsock, и падает именно создание
// сокета. Поэтому адрес для проверки не нужен вовсе, и она не зависит от
// наличия интернета на машине.
//
// Две неудачи различаются по-разному:
//
//	«connection refused» — сокет создан, стек сети работает: сеть ЕСТЬ,
//	порт просто закрыт;
//	любая другая ошибка — Winsock в контейнере не поднялся: сети НЕТ.
//
// Через net, а не через сырой syscall.Socket: сырой вызов требует
// предварительного WSAStartup и без него падает с ошибкой
// «application has not called WSAStartup» — ошибкой самой пробы, а не
// контейнера. На этом обманулись первые замеры.

import (
	"errors"
	"net"
	"os"
	"syscall"
)

// SelfCommandName — имя скрытого режима самопроверки сети.
const SelfCommandName = "internal-sandbox-netself"

// Self — самопроверка сети. Пишет результат в файл по пути из аргумента и
// возвращает код: 0 — сеть есть, 1 — сети нет.
func Self(resultPath string) int {
	_, err := net.Dial("tcp", "127.0.0.1:1")
	switch {
	case err == nil:
		writeResult(resultPath, "NET_OK")
		return 0
	case errors.Is(err, syscall.ECONNREFUSED):
		// Сокет создался и соединение дошло до сети: сеть есть.
		writeResult(resultPath, "NET_OK refused")
		return 0
	default:
		writeResult(resultPath, "NET_FAIL "+err.Error())
		return 1
	}
}

// writeResult — пишет результат в файл. Ошибка записи молча игнорируется:
// результат нужен для разбора, а падать из-за него нельзя.
func writeResult(path, text string) {
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(text), 0o600)
}
