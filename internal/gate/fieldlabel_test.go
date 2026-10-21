package gate

import "testing"

// TestFieldLabelDropsStepPrefix — в отчёте поле называется коротко.
//
// Полный путь `steps.parse.machine` отвечает на вопрос «откуда взялось», но
// не на вопрос «что это»: шаг и так напечатан заголовком. Сокращение должно
// быть только в отчёте — в журнале и API путь остаётся полным.
func TestFieldLabelDropsStepPrefix(t *testing.T) {
	cases := map[string]string{
		"steps.parse.machine":         "machine",
		"steps.disposable.disposable": "disposable",
		"steps.normalize.valid_count": "valid_count",
		"input.item":                  "input.item",
		"machine":                     "machine",
		"":                            "",
	}
	for in, want := range cases {
		if got := fieldLabel(in); got != want {
			t.Errorf("fieldLabel(%q) = %q, ждали %q", in, got, want)
		}
	}
}

// TestFieldValuePrintsStringsPlain — строка печатается без JSON-кавычек.
//
// Кавычки в отчёте перед человеком — шум, а не точность. Остальные значения
// остаются JSON: там структура важнее читаемости.
func TestFieldValuePrintsStringsPlain(t *testing.T) {
	if got := fieldValue("mx.yandex.ru"); got != "mx.yandex.ru" {
		t.Errorf("fieldValue(\"mx.yandex.ru\") = %q, ждали без кавычек", got)
	}
	if got := fieldValue([]string{"mx.yandex.ru"}); got != `["mx.yandex.ru"]` {
		t.Errorf("fieldValue(список) = %q, ждали JSON", got)
	}
	if got := fieldValue(42); got != "42" {
		t.Errorf("fieldValue(42) = %q", got)
	}
}
