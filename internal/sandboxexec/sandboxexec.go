//lint:file-ignore SA4023 Проверка «сравнение всегда истинно» срабатывает здесь
// только на платформе без изолятора: appcontainer.Run на ней отказывает всегда
// (заглушка возвращает ErrUnsupported), и `if err != nil` выглядит
// тождественно истинным. В сборке с изолятором (Windows) nil-ошибка — штатный
// путь, и проверка обязательна. Подавлено на уровне файла, а не строки:
// директива //lint:ignore обязана что-то подавлять и на Windows ругается
// «didn't match anything», то есть роняет CI на зелёном коде.

// Package sandboxexec — скрытый режим запуска внешнего кода в контейнере.
//
// Суть в том, что изоляция на Windows задаётся ПРАВАМИ САМОГО ПРОЦЕССА:
// нужен прямой CreateProcessW с атрибутом
// PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES, а os/exec такого не умеет —
// в списке атрибутов у него есть только PARENT_PROCESS. Поэтому
// контейнеризованный процесс создаёт не ядро WEDRA, а отдельный
// процесс-помощник: тот же самый бинарь, запущенный с одной узкой целью.
// На Linux та же роль у bubblewrap, только там это отдельная программа, а
// здесь — режим этого же бинаря.
//
// Пакет вынесен из internal/cli не по вкусу, а по необходимости: помощника
// должен уметь запускать и тестовый бинарь internal/plugin. Launcher для
// тестов — это сам тестовый бинарь (os.Executable()), и без перехвата в
// TestMain он ушёл бы в обычный прогон тестов вместо запуска контейнера.
// Цикла импортов при этом не возникает: cli → sandboxexec → appcontainer,
// и plugin → sandboxexec → appcontainer тоже.
//
// Помощник молчит. Его stdout — это stdout плагина, и туда пишет JSON
// плагина; любая служебная строка сломала бы контракт. Поэтому диагностика
// уходит в stderr, а отказ инфраструктуры — в отдельный код возврата.
package sandboxexec

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wykserdex/wedra/internal/appcontainer"
)

// FailureCode — код возврата помощника, когда контейнер не собрался.
//
// 201 выбран не случайно: коды 0..2 заняты (успех, отказ плагина, отказ
// инфраструктуры ядра), и отказ ПЕСОЧНИЦЫ не должен выглядеть в журнале
// как «плагин упал с кодом 201». Отдельный код позволяет отличить «плагин
// так и не стартовал» от «плагин отработал и упал».
const FailureCode = 201

// CommandName — имя скрытой подкоманды. Оно же часть контракта между
// ядром и помощником.
const CommandName = "internal-sandbox-exec"

// stringList — повторяемый флаг, собирающий список значений.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// Run — работа помощника. Возвращает код возврата.
//
// Отдельная функция без os.Exit внутри нужна тесту: подсистема должна быть
// проверяема без запуска процесса.
func Run(args []string) int {
	fs := flag.NewFlagSet(CommandName, flag.ContinueOnError)
	// Справка flag'ов уходит в stderr: stdout принадлежит плагину.
	fs.SetOutput(io.Discard)
	profile := fs.String("profile", "", "имя профиля контейнера")
	scratch := fs.String("scratch", "", "записываемый каталог запуска")
	cwd := fs.String("cwd", "", "рабочий каталог ребёнка")
	var reads stringList
	fs.Var(&reads, "read", "каталог, выдаваемый контейнеру на чтение и исполнение")
	allowNetwork := fs.Bool("network", false, "разрешить контейнеру выход в сеть (capability internetClient)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", CommandName, err)
		return FailureCode
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintf(os.Stderr, "%s: не задана команда\n", CommandName)
		return FailureCode
	}
	plan := appcontainer.Plan{
		Name:         *profile,
		Argv:         argv,
		Read:         appcontainer.Readable(reads),
		Scratch:      *scratch,
		WorkDir:      *cwd,
		Env:          env(*scratch),
		AllowNetwork: *allowNetwork,
	}
	res, err := appcontainer.Run(context.Background(), plan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", CommandName, err)
		return FailureCode
	}
	return res.ExitCode
}

// env — окружение контейнерного ребёнка.
//
// Помощник наследует окружение родителя, а родитель отдаёт плагину ровно
// untrustedBaseEnv плюс переменные scratch, — то есть переносить надо целиком
// то, что пришло.
//
// USERPROFILE и LOCALAPPDATA — исключение. AppContainer выводит путь профиля
// контейнера из этих переменных; если там останется путь НАСТОЯЩЕГО
// пользователя, контейнер получит ссылку на каталог, к которому у него всё
// равно нет доступа, а лишние переменные снаружи послужат зацепкой для
// побега. Поэтому обе принудительно указывают на scratch — это единственный
// каталог, который у контейнера точно есть.
func env(scratch string) []string {
	out := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			switch strings.ToUpper(kv[:i]) {
			case "USERPROFILE", "LOCALAPPDATA":
				continue
			}
		}
		out = append(out, kv)
	}
	out = append(out, "USERPROFILE="+scratch, "LOCALAPPDATA="+scratch)
	return out
}
