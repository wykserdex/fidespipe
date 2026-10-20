# Contributing to Fidespipe

Fidespipe is a young project with a public, compatibility-conscious contribution
process. Read [GOVERNANCE.md](GOVERNANCE.md) before changing protocol,
registry policy, CLI compatibility, or repository structure.

## Development workflow

1. Use Go 1.26 or newer and Python 3 for plugin fixtures. The floor is the
   `go` directive in `go.mod`, and CI builds the floor job on it: a toolchain
   older than the directive refuses to build the module, so the documented
   minimum is always a tested one.
2. Build the primary CLI with `go build -o fidespipe ./cmd/fidespipe`.
3. Run `go test ./... -count=1` and `go vet ./...`.
4. Keep generated run data under ignored `var/runs/`; never commit journals,
   `.fidespipe` locks, binaries, or local registry state.
5. Use `fidespipe` for new documentation and CI checks. `tool` is a compatibility
   command and should not define new behavior.
6. Record every product or CI change in [CHANGELOG.md](CHANGELOG.md), in the
   section of the current product version. See "Changelog entries" below — this
   step is part of the change, not a follow-up.

## Changelog entries

The rule exists because it was once missed on purpose and nobody noticed for
ten commits: code for an unreleased version keeps landing in `main` with no
binary behind it, which is exactly the state where the changelog quietly rots.

- **The entry goes into the section named by `VERSION`, not into `Unreleased`.**
  `Unreleased` is for work with no decided version yet. A version that has code
  in `main` but no binary has a section — it is the version being prepared.
- **A date is written only when the release actually exists.** In this file a
  date means "released": `0.33c - 2026-10-01` matches the GitHub release
  `v0.33c` of the same day. Work in progress is undated and uses an em dash
  with a descriptor (`0.31b — post-audit hardening (in progress)`). Adding a
  date to an unreleased section misrepresents the state of the repository.
- **CI and infrastructure changes are changelog material.** Timeouts, guard
  scripts, and test harnesses already have entries here. A change that only
  developers see is still a change someone has to be able to read about.
- **Never claim more than was measured.** If a fix is a hypothesis, a partial
  reproduction, or measured on a host that is not the reference one, say so in
  the entry. Entries written for the 0.34 sandbox work say which Windows build,
  that the probe is not committed, and that the hang was not reproduced rather
  than fixed.
- **After any plugin change, regenerate the pins and the trust seed** —
  `registry validate --local-source=.`, `go run ./internal/plugin/cmd/genseed`.
  Trust is granted by tree hash against the pin, so a stale pin installs old
  code under a trusted label.

## Product and protocol versions

The product version is the release value in the root `VERSION` file. The
protocol version is independent and lives in `protocol/VERSION`; the complete
rules are in [docs/versioning.md](docs/versioning.md). Do not reuse a released
tag or infer a new product version from an old `v9`/`v10` alias.

A protocol, registry, or layout change requires a public proposal, a migration
note, and compatibility tests. The canonical repository layout is documented
in [docs/architecture.md](docs/architecture.md) and is frozen against parallel
renames until an accepted ADR.

## Registry admission

The registry is `registry.yaml` in the repository root, with schema version
`0.1`. A plugin or preset enters the supported registry only after green CI,
including `fidespipe registry validate`.

### Plugin checklist

1. **Манифест** `plugin.yaml`:
   - `id` — обязан совпасть с именем записи в реестре (CI проверяет);
   - `runtime`, `input`/`output` порты;
   - **`permissions` — честно**:
     - `network` — то, что **исполнимо**, а не то, что хотелось бы:
       - плагину не нужна сеть → `network: []` (пусто = сети нет; `network: deny`
         пайплайна его запретит);
       - плагину нужен интернет → `network: [ { any_host: true, port: 443,
         note: "target: api.example.com:443" } ]`. `any_host: true` — это
         **разрешение на весь интернет**, и `note` существует ровно для того,
         чтобы настоящая цель была видна в аудите;
       - `network: [ { host: "api.example.com", port: 443 } ]` **без** `any_host`
         — не ограничение, а ловушка: точечного egress-фильтра по host:port нет
         ни на одной платформе, поэтому такой плагин **не запустится ни в одном
         пайплайне** (`E_NETWORK_NOT_ENFORCEABLE` — и при `allow`, и при `deny`).
         `plugin validate` предупредит об этом сразу; обходить предупреждение,
         дописав `any_host`, не надо — надо решить, нужен ли плагину интернет;
     - `secrets: [ENV_KEY]` — КАЖДЫЙ env-ключ, который плагин читает.
2. **Конформность** `plugin.test.yaml` — минимум 3 кейса:
   - happy path (`status: ok`, поля вывода);
   - доменная ошибка (`error.code`, `retryable` — ожидаемые значения);
   - битый JSON на входе (exit 2 — платформенная ошибка).
   Реальных ключей в тестах быть не должно — mock-режим (паттерн LLM-плагинов:
   `LLM_MOCK=1`), сеть — только если тестируемая (MX-запись и т.п.).
3. Локальные проверки:
   ```
   fidespipe plugin validate <dir>
   fidespipe plugin test <dir>
   ```
4. **PR**: каталог `plugins/{official,community}/<name>/` + запись в
   `registry.yaml` (`source`, `path`, `version` = тег текущего релиза,
   `description` — что делает, одной строкой).
5. CI: `registry validate` прогоняет манифест, id, **все** конформные тесты.
   Красный = в реестре не оказаться.

### Preset checklist

1. `examples/<name>.yaml`:
   - `format_version`, `steps`;
   - `secrets: [KEY]` — все ключи, которые цепочке нужны;
   - `network: deny` — если цепочка сетью пользоваться не должна.
2. `fidespipe pipeline validate examples/<name>.yaml` — зелёный.
3. **PR**: файл + запись пресета в `registry.yaml`.

### Reviewer checklist

- `permissions` честные: код делает ровно то, что заявлено в манифесте (и не больше).
- Сеть заявлена исполнимо: либо пусто, либо `any_host: true` с настоящей целью в
  `note`; объявление по `host:port` без `any_host` — блокирующее замечание.
- В коде/тестах/примерах — ни одного значения секрета (только имена).
- Ошибки: доменные vs платформенные, `retryable` по смыслу.
- `description` — что делает и что нужно, без маркетинга.

## Version and security links

- Product version: root `VERSION` and [docs/versioning.md](docs/versioning.md).
- Protocol version: `protocol/VERSION` and `protocol/CHANGELOG.md`.
- Registry source pins: `registry.yaml` (`version` + `commit`), never `main`.
- Security reports: [SECURITY.md](SECURITY.md).
- Governance and breaking changes: [GOVERNANCE.md](GOVERNANCE.md).
