# Fidespipe

[Русский](README.md) | English

**Checks put together out of other people's tools: a domain, an email address, a
phone number, a leak in code, a PE file — one command, a verdict at the end.**

The tools are other people's: written and fixed by people who don't know each
other. Fidespipe brings them to a common shape — JSON in, JSON out — keeps a
journal of every run, and hands the debatable cases to a person instead of
deciding for them.

```bash
fidespipe pipeline install email_check    # install the check and its plugins
fidespipe pipeline run email_check.yaml   # run it; the inputs live in the YAML
```

## Checks that are already put together

| check | what it does | what it shows |
|---|---|---|
| `email_check` | syntax, disposable domain, MX records | the address's MX records, and whether it is disposable |
| `phones_audit` | a batch of phone numbers in E.164 | how many are valid, how many are not, and the invalid ones |
| `iban_check` | the IBAN check digits | whether the account is valid, the formatted number, the bank code |
| `secrets_scan` | secrets in code | how many findings, the detector version, the findings themselves |
| `pe_triage` | a PE file: architecture, sections, imports | bitness, section count, imported DLL count |
| `near_dupe_check` | two strings for near-duplicate | distance, similarity, and whether they count as similar |
| `email_triage_chain` | email → triage → a person decides | risk and verdict, which a person edits by hand |
| `csv_load` | CSV → load → gate | row count, headers, the rows themselves |

What comes out are fields and values, not a finished report: the "is this fine or
not" decision belongs to a person, and in `email_triage_chain` they edit the
verdict by hand.

There are also mechanics examples — `parallel_demo`, `foreach_step_demo`,
`when_demo`, `gate_demo`, `csv_foreach`, `csv_foreach_summary`,
`dir_snapshots`, `llm_text_chain`. They show branches, loops, conditions and the
gate, not a task; you do not need them to work.

**An agent is not the point, it is one of the ways in.** The same checks and the
same gate are available from the terminal, from a browser, and over MCP.

## Why this works

The one thing you cannot get for free is an agreement between other people's
tools. Everything else is built on top of it:

| | what this gives you |
|---|---|
| **A contract at the boundary** | plugin inputs and outputs are checked **before** the run; a broken manifest does not blow up halfway down a chain |
| **A run journal** | every run stays on disk: events, context, artifacts; a broken run can be resumed where it stopped |
| **A human in the loop** | `core/human_gate` waits for a **real** decision, with a ceiling on waiting and approval by a signed link from any device |
| **Trust by content hash** | "id@sha256:…": edit the file, lose the trust; the plugin's name earns nothing |
| **Isolation** | untrusted code goes into a sandbox: bubblewrap on Linux, AppContainer on Windows (the plugin directory is read-only, the network is denied) |
| **A catalog with donors** | 100 plugins, a store that installs them, and typed recipes for Go programs, binaries and scripts with a sha256 check |
| **A writing tool** | a browser editor for plugins, and `fidespipe plugin import` — reading an external Python project into a ready plugin |
| **A scheduler** | a local daemon with a **durable queue**: a slot that ran once never runs twice; interrupted work is not silently replayed |
| **Deployment** | Docker and systemd, for a server where checks run on a schedule |

```text
plugin A → plugin B → [a person decides] → plugin C → result
```

The points Fidespipe can be extended through:

| Surface | Tools |
|---|---|
| Agents (MCP) | 8 tools: `list_plugins`, `describe_plugin`, `validate_pipeline`, `plan_pipeline`, `run_pipeline`, `get_run`, `cancel_run`, `exec_plugin` (off by default) |
| Plugins | your own process with JSON in and out; the contract is in [PROTOCOL.md](protocol/v0.2/PROTOCOL.md), the version in [protocol/VERSION](protocol/VERSION) |
| The registry | a plugin record passes the conformance gate and lands in `registry.yaml`; the `registry` step in CI checks it |
| Import | someone else's Python project becomes a plugin via `fidespipe plugin import` |

## Current contract

- Product version: `0.35.0` from [`VERSION`](VERSION)
- Pipeline/plugin protocol: `0.2` from [`protocol/VERSION`](protocol/VERSION)
- Primary CLI: `fidespipe`
- Compatibility CLI: `tool` (legacy surface; do not add new behavior there)

The product version and protocol version are independent. See the
[versioning policy](docs/versioning.md).

## Build and run

```bash
go build -o fidespipe ./cmd/fidespipe
./fidespipe version
./fidespipe plugin list
./fidespipe pipeline validate examples/email_check.yaml
./fidespipe pipeline run examples/gate_demo.yaml
./fidespipe runs list
```

Building from source needs Go 1.26 or newer. That floor is the `go` directive in
`go.mod`, and the `go-floor` CI job builds on exactly it, so the documented
minimum is always the tested one.

There are 100 plugins in the repository and in the registry: 5 `official` and 95
`community`. Of those, 65 need nothing beyond a bare Python 3; another 35 declare
a donor through `runtime.requires`, and `install` puts it in place by itself —
exactly `pip install package==version`.

Five plugins have no donor on PyPI, and it is installed from a **typed recipe**
in the manifest:

```yaml
runtime:
  donor:
    kind: go        # go | binary | script
    name: gospider
    version: v1.1.6
    module: github.com/jaeles-project/gospider
    bin_env: GOSPIDER_BIN
```

Three kinds: `go` (`go install module@version`, checksums verified by the Go
proxy), `binary` (a ready-made file: `url` + `sha256`), `script` (`url` +
`sha256` + an interpreter). **A sha256 is mandatory** for `binary` and `script`,
and it is checked *before* unpacking: downloading executable code without
verifying its hash is not an install, it is a backdoor. The address must be
`https`.

The donor is installed **outside the plugin directory**, in
`<plugins>/.donors/<id>/`. That is not a matter of taste: trust is granted by
the hash of the directory's contents, so installing a donor inside it would
change the hash and the plugin would stop being trusted at the very moment it
was installed.

The numbers above are a property of the catalog. The state of any given machine is
measured, not asserted: `fidespipe doctor` recomputes every plugin with the same Python
that runs Fidespipe itself, and prints how many are ready and what to install. It
installs nothing and runs nothing.

**About the word "community":** all 100 plugins are currently written
by the repository owner and live in that same repository — all 100 registry
entries carry the same `source`, `https://github.com/wykserdex/fidespipe`, with no
outside sources and no third-party authors. "Community" means shipped outside the
core, that is, everything not in the maintained `plugins/official/` set — not
"written by someone else". The `author` field in the manifests is still a
placeholder (`community`, `community · fidespipe showcase (OSINT)`, `external
author #4 …`) rather than a person's name; the `official` manifests do not have
that field at all.

The primary CLI supports pipeline validation and execution, plugin registry
operations, resume, the local GUI, and an MCP stdio adapter. Plugin manifests
declare their own component version and protocol compatibility separately.

## Boundaries

Honest limits, so you don't have to guess.

- **Everything happens on one machine.** Orchestration is a DAG inside a single
  process: steps, `when`, `foreach`, `parallel_group` with a barrier. There is no
  distributed scheduler, no queues between machines, no service discovery.
- **The scheduler is local.** `fidespipe daemon` ticks once a minute and starts
  whatever the cron says is due, out of a **durable queue**: a slot is claimed
  before the run, so a daemon restart does not run the same slot twice, and work
  caught in flight by the daemon's death is marked interrupted rather than
  replayed. There are still no inbound triggers: scheduling is by time only.
- **It is a binary, not a cloud.** No accounts, no telemetry, and no server that
  holds your data for you: a `plugins/` directory and journals in `var/runs/`.
  The console listens on `127.0.0.1` by default; exposing it is a deliberate
  operator flag (`--listen`, `--allow-remote`). External services connect as
  ordinary plugins with your key from an environment variable, not as
  "Fidespipe integrations".
- **A plugin lands in the registry because it passes the conformance gate**, not
  because it was written: it has to declare its `permissions` honestly. The 100
  plugins are not a showcase — 65 run on a bare Python 3, 35 declare a donor, and
  `install` puts the donor in place itself.

## Pipeline format stability

**What the format version means.** `format_version` in YAML is the version of the
*format protocol*, not the version of the application. The axes are independent:
the product version lives in the `VERSION` file, the protocol version lives in
`protocol/VERSION` and moves rarely. It is checkable: `fidespipe pipeline validate`
on a file with an unknown `format_version` returns the **error**
`E_FORMAT_VERSION`, not a warning, and an empty field is the warning
`W_FORMAT_VERSION_MISSING`. The supported values are listed in code and do not
grow silently — today exactly `"0.1"` and `"0.2"`.

**What is promised.** The format is not frozen, but the commitment is specific:

> As long as the `format_version` in your file has not changed, a pipeline that
> validated keeps validating and keeps running.

The promise rests on things that can be checked rather than believed:

- **Two values, two live formats.** `"0.1"` is not declared legacy: it is
  accepted on equal terms with `"0.2"`, pipelines using it live in `examples/`,
  and the `pipelines` step (`validate` + `lint` + `plan`) of a full `fidespipe check`
  runs over them — the same run CI does.
- **A breaking edit cannot be silent.** Per
  [GOVERNANCE.md](../GOVERNANCE.md), a protocol change requires a public
  proposal stating its compatibility impact, migration steps, tests, and the
  version axis it changes; [CONTRIBUTING.md](../CONTRIBUTING.md) additionally
  requires a migration note and compatibility tests. The outcome has to show up
  in `protocol/CHANGELOG.md` together with the new value in
  `protocol/VERSION` and a `protocol/vX.Y/` directory.
- **The contract is checked mechanically.** The `errcodes` gate compares the
  error codes in Go with `protocol/v0.2/ERRORS.md`, the `versions` gate compares
  the versions in README and docs with the repository, and the `schemas` gate
  compares `schemas/` with the examples and manifests.

**What can still change** — the honest list, without which the promise above
would be empty:

- **The format grows.** New optional fields and new error codes can appear —
  that has already happened (`snapshot_lost`, `E_AGENT_EXEC_AUDIT`). Old YAML does
  not break because of it.
- **There is no compatibility in the other direction.** Pipeline parsing is
  strict (`KnownFields(true)` in `internal/pipeline/parser.go`): a Fidespipe binary
  older than your file refuses to open it rather than ignoring what it does not
  know. An old binary reads old files, not new ones.
- **Error codes and MCP tool names are not a frozen API.** The gates keep the Go
  codes, the published contract, and this README in agreement; that is a
  consistency check, not a ban on adding or renaming.
- **A run depends on the plugins.** If a plugin left the registry or its donor
  tool is not installed, the chain does not run. That is not about the format.
- **The promise does not cover the CLI or the interface.** Command flags, the
  MCP tool set, and console behavior live on the application axis and change
  with a release.
- **The format is not frozen forever.** The project is on the 0.x line, and
  [SECURITY.md](../SECURITY.md) states plainly that only the current line
  receives fixes. A new protocol version is allowed — it just has to be
  announced as described above.

Full breakdown with code references: [docs/format-compatibility.md](docs/format-compatibility.md).

The local console (`fidespipe gui`) shows runs with a timeline, pipelines with a DAG,
the plugin catalog, and a visual editor:

![Building a chain and running it: home, editor with linked steps, step properties, the chain as YAML and a run](demo.gif)

The full console tour is in [`docs/demo.gif`](docs/demo.gif).

## Repository layout

```text
cmd/fidespipe/            primary CLI
cmd/fidespipegui/         desktop launcher
cmd/tool/             compatibility CLI
internal/pipeline/    model, parsing, validation, planning
internal/execution/   runner and resume
internal/plugin/      plugin process and manifest contract
internal/registry/    registry and install pins
internal/journal/     append-only run journal
internal/gate/        human gate
internal/runctx/      shared context
plugins/official/     maintained core plugins
plugins/community/    everything outside that maintained core
examples/             canonical examples and presets
conformance/fixtures/ public conformance corpus
var/runs/             runtime output, ignored by git
```

`internal/core` is a transitional compatibility layer. The layout and its
compatibility surfaces are documented in [architecture](docs/architecture.md)
and are not to be reorganized without a public proposal and migration plan.

## Screenshots

| | |
|---|---|
| ![Home screen](docs/screenshots/menu.png) | ![Run: timeline and context](docs/screenshots/run-detail.png) |
| **Home** — what to run next | **Run** — step timeline, live journal and context |
| ![Pipeline and DAG](docs/screenshots/pipeline-dag.png) | ![Plugin card](docs/screenshots/plugin-detail.png) |
| **Pipeline** — YAML, DAG and validation before launch | **Plugin** — description, permissions and dependencies |
| ![Visual editor](docs/screenshots/editor.png) | |
| **Editor** — build the chain by dragging, round-trip through the core | |
| ![Settings: standard density](docs/screenshots/prefs-standard.png) | ![Settings: compact density](docs/screenshots/prefs-compact.png) |
| **Settings, standard** — density, accent, node roles, rail width | **Settings, compact** — the same screen at a smaller type size and tighter padding |

## Maintainers and safety

Fidespipe is currently maintained by one primary maintainer. The project is
transparent about that risk and uses public proposals, CI gates, immutable
release tags, and explicit registry admission. See [governance](GOVERNANCE.md)
and [contributing](CONTRIBUTING.md).

Plugin permissions are declarations, not an operating-system sandbox. Review
network, filesystem, and secret permissions before installing a plugin.
A network permission is either every host (`any_host: true`) or no network at
all: there is no per-host egress filter on any platform (see
[SECURITY.md](SECURITY.md#egress-filtering-proxy-built-kernel-enforcement-still-missing)).
Isolation for untrusted external code works on Linux (bubblewrap). On Windows
AppContainer is measured and proven, but **not enabled**: switching the security
boundary needs its own hostile tests, not a "it works by measurement". On macOS
such code is refused.

A plugin may declare `sandbox: untrusted` in its manifest. Such a plugin only
runs inside an OS sandbox and only with explicit operator consent via
`--allow-untrusted-plugins`; `--deny-untrusted-plugins` refuses every plugin in
the run and is the recommended flag for CI. On Linux the backend is `bwrap`
(read-only filesystem, separate PID/IPC/UTS namespaces, and a network namespace
that is always its own - egress comes from a userspace stack, `slirp4netns`, and
without it a plugin that declared `permissions.network` is refused rather than
run). On macOS and Windows there is no isolation backend, so untrusted code is
refused outright; the former `sandbox-exec` implementation was archived out of
the tree (it is in git history at commit 9d17120) and is not built. A backend that is installed
but cannot isolate on the host (for example, user namespaces are blocked) counts
as absent. The sandbox restricts writes but not reads, and does not filter egress
by destination for a plugin that declares `permissions.network` - it gets
unfiltered egress, just not inside the host's network namespace. So it is not a
complete boundary for hostile code. Report vulnerabilities according to
[SECURITY.md](SECURITY.md).
