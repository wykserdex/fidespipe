# Demo: агент + fidespipe + человек (≈3 мин)

Сценарий для видео/GIF в README. Материал готов: `examples/email_triage_chain.yaml`
(после фикса 0.2 даёт 3 ok / 1 aborted), `plugins/official/*`, `examples/csv_*`.

## 1. Просьба (Claude Code / Cursor)

> «Проверь эти email и подготовь триаж»

Агент вызывает MCP:

- `list_plugins` → находит `syntax_mx_checker`, `disposable_checker`, `email_triage`
- пишет YAML пайплайна с `foreach: input.emails`, `item_type: string`, `item_format: email`

## 2. Агент ошибается — fidespipe подсказывает кодами

Первый YAML с битой привязкой:

- `validate_pipeline` → `ok:false`, `issues[0].code == "E_TYPE_MISMATCH"`,
  `fix.candidates: ["steps.syntax.email", ...]`
- агент **сам исправляет** bind по candidates, повторяет `validate_pipeline` → `ok:true`
  (предупреждение `W_FOREACH_ITEM_FORMAT` про `"bad-email"` — не блокирует:
  плохой элемент уронит только себя)

## 3. Запуск — ждёт человека

- `run_pipeline` → `{run_id, status: "waiting_human"}`
- `fidespipe mcp` сам открывает браузер человека на этом ране (консоль гейтов на
  127.0.0.1, ссылка с одноразовым кодом входа; агенту уходит адрес без кода),
  человек правит `risk` в гейте `review` и жмёт accept
- `get_run` → `done`: `3 ok / 1 aborted` (`"bad-email"` abortнул только свой элемент),
  выходы шагов + ссылка на журнал

## 4. Финальный кадр: агент не может одобрить сам

```bash
curl -X POST http://127.0.0.1:<порт из hint>/api/runs/<id>/gate -d '{"action":"accept"}' # → 401 E_SESSION_REQUIRED
```

- у агента нет инструмента одобрения (MCP: только `get_run` с hint
  «попросите пользователя одобрить шаг X в окне fidespipe»)
- и агент не может выкинуть гейт: пайплайн без `core/human_gate`, где есть шаг
  с capabilities «сеть/диск/секреты», отклоняется с `E_GATE_REQUIRED` до старта
- `--yes` при `approval: human` / `pipeline.gates: human_only` тоже ждёт человека
- в журнале `gate_decision`: `source: gui` (терминал: `terminal`, CI: `auto_yes`)

## Повторить локально

```bash
go build -o fidespipe ./cmd/fidespipe
./fidespipe pipeline validate --json examples/email_triage_chain.yaml
./fidespipe pipeline run examples/email_triage_chain.yaml --yes
# → ok=3 aborted=1
./fidespipe plugin test --conformance internal/core/testdata/plugins --json
# → ok:true (handshake, big_stdout, big_stderr, cancel, error_codes)
```
