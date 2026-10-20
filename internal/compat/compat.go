// Package compat — совместимость с прежними именами после переименования.
//
// Переименование продукта меняет имена переменных окружения, а переменные
// окружения — это интерфейс: их ставят в CI, в скриптах, в ручной настройке.
// Молча перестать их читать — значит сломать чужую настройку без
// предупреждения.
//
// Поэтому прежние имена поддерживаются: значение из старой переменной
// переносится в новую, если новая не задана. Явная новая переменная всегда
// побеждает — тот, кто её поставил, знает, чего хотел.
//
// Совместимость не вечная: она нужна на переходный период, пока у людей в
// CI и в скриптах стоят старые имена. Убрать её — отдельное решение, и оно
// должно быть отдельным коммитом с записью в CHANGELOG.
package compat

import "os"

// pair — новое имя и прежнее имя одной переменной.
type pair struct {
	new string
	old string
}

// pairs — переменные, чьи имена изменились при переименовании.
var pairs = []pair{
	{"FIDESPIPE_TRUST_CONFIG", "WEDRA_TRUST_CONFIG"},
	{"FIDESPIPE_DONOR_ROOT", "WEDRA_DONOR_ROOT"},
	{"FIDESPIPE_PLUGINS_DIR", "WEDRA_PLUGINS_DIR"},
	{"FIDESPIPE_NETWORK", "WEDRA_NETWORK"},
	{"FIDESPIPE_APPROVE_URL", "WEDRA_APPROVE_URL"},
	{"FIDESPIPE_APPROVE_LISTEN", "WEDRA_APPROVE_LISTEN"},
	{"FIDESPIPE_TRUST_SECRET", "WEDRA_TRUST_SECRET"},
	{"FIDESPIPE_TRUST_PROBE", "WEDRA_TRUST_PROBE"},
	{"FIDESPIPE_TEST_SECRET_DO_NOT_SET", "WEDRA_TEST_SECRET_DO_NOT_SET"},
	{"FIDESPIPE_TEST_DECLARED_SECRET", "WEDRA_TEST_DECLARED_SECRET"},
	{"FIDESPIPE_TEST_UNRELATED_SECRET", "WEDRA_TEST_UNRELATED_SECRET"},
	{"FIDESPIPE_MOCK", "WEDRA_MOCK"},
}

// Init переносит значения прежних переменных окружения под новые имена.
//
// Вызывается один раз при старте программы, до того как что-либо прочитает
// окружение. Функция идемпотентна: повторный вызов ничего не портит.
func Init() {
	for _, p := range pairs {
		if os.Getenv(p.new) != "" {
			continue
		}
		if v := os.Getenv(p.old); v != "" {
			_ = os.Setenv(p.new, v)
		}
	}
}
