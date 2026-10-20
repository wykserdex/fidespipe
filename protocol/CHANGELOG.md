# Протокол — история версий

## v0.2 (текущая)

References to application versions below are historical release notes; the
current protocol source of truth is `protocol/VERSION`.

Расширение v0.1, обратно совместимо (изменения записаны по мере выхода, с
версией приложения, где появились):

- **v0.33a (app): агент исполняет плагины напрямую — четыре кода ошибок
  (ERRORS.md)**: `E_AGENT_EXEC_DENIED`, `E_AGENT_PLUGIN_UNTRUSTED`,
  `E_AGENT_EXEC_BUSY`, `E_AGENT_EXEC_AUDIT`. Новый инструмент `exec_plugin`
  в MCP-хосте. Формат pipeline, манифест плагина и фикстуры конформности не
  тронуты, поэтому версия протокола остаётся 0.2: ни одна конформная
  реализация не зависит от кодов этого хостового инструмента.
  - **Журнал запусков**: `<runs-dir>/agent-exec.jsonl`, две строки на запуск
    (`agent_plugin_exec_intent` ДО запуска, `agent_plugin_exec` после),
    связаны `exec_id`. Не записалось намерение — плагин не запускается
    (`E_AGENT_EXEC_AUDIT`)
- потеря снапшота контекста больше не молчит: событие `snapshot_lost`
  (`reason`, `bytes`) + терминальный `run_failed` с кодом `snapshot_lost`
  вместо `run_end`; причина — только «почему» и «сколько байт», без данных
  контекста и секретов. Аддитивно, обратно совместимо (ERRORS.md)
- `bind:` в шаге пайплайна перекрывает `from` манифеста (PROTOCOL §9)
- Манифест без `from` легален: «привязку обязан дать пайплайн»
- `bind` на несуществующий порт — ошибка валидации (защита от опечаток)
- `optional: true` порт без привязки = warning, не error (fix #10)
- `file_ref` warning до запуска, если относительный путь не резолвится от cwd
  плагина, с подсказкой «есть ли от корня»
- `format` только для строк, проверяется в `plugin validate` (fix #8)
- `format_version` теперь `0.1` или `0.2`, опечатка — ошибка, не warning+OK
  (fix #13)
- pipeline-`foreach` по `steps.<id>.<field>` (pre-фаза, app v0.12)
- **v0.20 (app): управляющий поток на уровне шага (PROTOCOL §12)** —
  `when:`, `foreach:` на шаге, `parallel_group`
- **v0.8a (app): фикс DnD редактора (mouse-события вместо нативного HTML5); протокол без изменений**.
- **v0.8 (app): desktop-exe (cmd/fidespipegui, WebView2, чистый Go); протокол без изменений**.
- **v0.7 (app): GUI вшит в бинарник (go:embed) — release-артефакты автономны; протокол без изменений**.
- **v0.6 (app): редактор управляет pipeline.network (политика allow/deny); протокол без изменений**.
- **v0.5 (app): редактор управляет pipeline.secrets (env-ключи плагинов); протокол без изменений**.
- **v0.4a (registry): записи crtsh/the_harvester с SHA-пинами на v0.4 · v0.4: community-плагины crtsh (встроенный stdlib-HTTP) + the_harvester (обёртка CLI); протокол без изменений**.
- **v0.3a (registry): записи maigret/holehe с SHA-пинами на v0.3 (формат реестра не меняется) · v0.3: community-плагины maigret/holehe (обёртки внешних OSINT-CLI, declare-now сеть); протокол без изменений**.
- **v0.29 (app): редактор управляет `retry` (on_error: retry + attempts/delay/backoff); протокол без изменений**.
- **v0.28a: реестр (registry v0.1): аддитивное поле `commit` (SHA-пин; install fail-closed при рассинхроне с тегом). Протокол пайплайнов без изменений**.
- **v0.28 (app): редактор управляет `foreach`/`parallel_group`/`after_foreach` (шаг) и pipeline-`foreach`; протокол без изменений**.
- **v0.27 (app): редактор управляет `when` (10 операторов ядра); протокол без изменений**.
- **v0.26a (app): редактор сохраняет `format_version` исходного файла (новое doc → 0.2); протокол без изменений**.
- **v0.26 (app): имя** — продукт/бинарник Fidespipe (`fidespipe`), модуль `fidespipe`.
- **v0.24 (app): браузерный гейт** — событие `gate_wait` (step, form с
  текущими значениями, actions) до блокировки на гейте; `gate_retry` —
  мусорное решение (≤5, как в терминале); `gate_decision` получил
  `skipped_edits` (правки с неверным типом — пропущены, список в событии)
- **v0.23 (app): контракт рантайма** — типы и форматы выходов (и входов)
  проверяются после каждого запуска (до этого — только наличие); несовпадение
  = нарушение контракта, шаг падает. Форматы: text/email/url/ip/file_ref
- secrets (app v0.16), network declare-now (app v0.17, PROTOCOL §11)

## v0.1

Изначальный протокол (замер в M1–M3):

- Плагин — процесс: stdin JSON → stdout JSON конверт
- Manifest с обязательным `from` для каждого input-порта
- `format` — подтип строк: text, email, url, ip, file_ref
- Exit codes: 0 ok, 1 domain, ≥2 platform
- Shared Context: `input.*` и `steps.<id>.<field>`
- Enforce: незадекларированные выходы отбрасываются (warning), обязательные
  без значения — ошибка
- Политики: stop|skip|retry, foreach per-item scope
- human_gate: form с полями, материализация под `steps.<gate>.*` по basename

## Правила версионирования

- `platform_api` в plugin.yaml — semver-диапазон совместимости с ядром (`^0.1`)
- `format_version` в pipeline.yaml — `0.1` или `0.2`
- `protocol_version` в конверте — `0.2`; `0.3` принимается только как legacy input
- Обратная совместимость: v0.2 ⊃ v0.1 (from остаётся дефолтом)
