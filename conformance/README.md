# Conformance tests

Проверяют, что плагин и ядро соблюдают PROTOCOL, а не только что manifest валиден.

Фикстуры из `conformance/fixtures/v0.2/` (в development checkout также доступны в `internal/core/testdata/plugins/`):
- `echo_ok` — корректный ok
- `failer` — доменная ошибка exit 1
- `crasher` — краш exit 2
- `bad_proto` — мусор в stdout → protocol_violation
- `contract_breaker` — не возвращает обязательное поле
- `leaker` — возвращает незадекларированное поле
- `type_drifter` — дрейф типа
- `file_ref_echo` — file_ref
- `sleeper` — отмена
- `retry_flaky` — retryable
- `chatter` — oversized stdout
- `big_stderr` — большой stderr без нарушения протокола
- `num_only` — число на входе и выходе
- `consumer_opt` — optional-вход отсутствует
- `net_probe` — доходит ли `FIDESPIPE_NETWORK=deny`
- `net_demo` — объявляет сеть host:port (проверяется политикой, не исполняется)

Проверки:
- загрузка и строгая проверка manifest каждой фикстуры (`fixture_manifests_loaded_N`)
- корректный handshake (stdin JSON → stdout JSON)
- аварийное завершение (exit >=2) → platform:* (`crash_exit2`)
- мусор в stdout → protocol_violation (`bad_proto`)
- незадекларированное поле отбрасывается (`undeclared_output`)
- дрейф типа и отсутствие обязательного поля (`type_drift`, `required_output`)
- number round-trip, optional-вход, `FIDESPIPE_NETWORK`, file_ref warning
- доменная ошибка не выглядит как авария; `retryable: true` доезжает до ядра
- timeout и отмена
- лимит stdout и stderr
- golden issue codes

Про покрытие: чек `fixture_coverage` перечисляет, сколько фикстур исполнено
и какие пропущены осознанно (с причиной). Фикстура, добавленная в каталог и
не исполненная батареей, роняет этот чек — иначе отчёт снова будет считать
неисполненное покрытым.

Запуск: `go test ./internal/core/ -run 'TestPluginTest|TestExec' -v -count=1`
или батарея CLI: `fidespipe plugin test --conformance --json`
или одна фикстура: `fidespipe plugin test conformance/fixtures/v0.2/<name>`
