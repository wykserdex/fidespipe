package plugin

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// Политика доверия к коду плагина. Плагин приходит извне (community registry,
// локальная папка пользователя) и исполняется с правами пользователя: скрипт
// может прочитать любой файл, доступный процессу, и утащить env. Поэтому
// запуск внешнего кода должен быть осознанным решением ядра, а не полем,
// которые плагин пишет в своём манифесте.
//
// Политика живёт в context, чтобы не менять сигнатуры всех Exec-вариантов:
// ран (runner) ставит политику один раз, все шаги наследуют её через ctx.

// TrustPolicy — решение ядра о том, чему можно исполняться.
type TrustPolicy struct {
	// DenyUntrusted — все плагины считаются внешними, включая те, что не
	// объявили sandbox: untrusted. Включается флагом
	// --deny-untrusted-plugins: полезно для CI и рандов с community-плагинами.
	DenyUntrusted bool
	// AllowUntrusted — явное согласие запускать внешний код. Плагин всё равно
	// уходит в изолятор; если изолятора на хосте нет, запуск падает
	// (fail-closed), а не выполняется без песочницы.
	AllowUntrusted bool
	// Trusted — allow-list доверенных плагинов оператора (id → хэши
	// содержимого). Это единственный источник доверия: поле `sandbox` в
	// манифесте может только ПОНИЗИТЬ его, никогда не повысить.
	//
	// nil означает «allow-list не задан» и равносилен пустому: доверенных
	// плагинов нет. Подставлять BuiltinAllowList() по умолчанию нельзя —
	// иначе пустой ctx (Exec без политики) выглядел бы доверенным, и ошибка
	// «allow-list не передан» маскировалась бы зелёным.
	Trusted *AllowList
	// AgentCanExec — агент может самостоятельно запускать плагины через
	// exec_plugin (MCP). По умолчанию false: без явного --allow-agent-exec
	// инструмент отказывает, ничего не запуская. Каждый запуск пишется в
	// <runs-dir>/agent-exec.jsonl с source: agent_auto, то есть решение агента
	// о запуске кода остаётся читаемым в журнале.
	//
	// Оговорка про гейт: одобрения человеком exec_plugin НЕ спрашивает. Гейт
	// в проекте — это точка входа для human_gate внутри РАНА, а exec_plugin
	// рана не создаёт; блокировать MCP-вызов на живого человека здесь означало
	// бы, что агент не сможет работать без человека вовсе. Структурную границу
	// даёт не гейт, а политика: плагин из agent-plugins/ всегда untrusted, и
	// для его запуска нужен ещё и --allow-untrusted-plugins.
	AgentCanExec bool
	// AgentCanWritePlugins — агент может создавать новые плагины в
	// выделенный каталог (agent-plugins/). Основной plugins/ не затрагивается.
	// Написанное агентом считается untrusted по построению.
	AgentCanWritePlugins bool
}

// AgentPluginDir — каталог для плагинов, написанных агентом.
// Всегда внутри workdir, всегда untrusted.
const AgentPluginDir = "agent-plugins"

// IsAgentWrittenPlugin — плагин написан агентом (лежит в agent-plugins/).
//
// Сравнение идёт ПО КОМПОНЕНТАМ пути, а не по подстроке. Подстрока давала
// ошибки в обе стороны, и обе опасны:
//
//   - «backups/agent-plugins», «plugins/agent-plugins-x/»,
//     «plugins/mailer-agent-plugins/» — считались написанными агентом;
//   - на Windows «Agent-Plugins\» и «AGENT-PLUGINS\» это ТОТ ЖЕ САМОГО
//     каталог, а строковое сравнение регистрозависимо, поэтому плагин,
//     написанный агентом, признавался обычным, то есть ДОВЕРЕННЫМ. Это
//     прямо противоречит «написанное агентом — untrusted по построению».
//
// Направление ошибки выбрано намеренно: любая компонента, равная
// agent-plugins, делает плагин агентским. Ложное срабатывание стоит лишней
// песочницы, ложное отсутствие — обхода доверия.
func IsAgentWrittenPlugin(m *Manifest) bool {
	if m == nil || m.Dir == "" {
		return false
	}
	want := filepath.ToSlash(filepath.Clean(AgentPluginDir))
	equal := func(a, b string) bool { return a == b }
	if runtime.GOOS == "windows" {
		// Файловая система Windows регистр не различает: различать должны и мы.
		equal = strings.EqualFold
	}
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(m.Dir)), "/") {
		if part != "" && equal(part, want) {
			return true
		}
	}
	return false
}

type trustPolicyKey struct{}

// WithTrustPolicy возвращает ctx с политикой доверия.
func WithTrustPolicy(ctx context.Context, p TrustPolicy) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, trustPolicyKey{}, p)
}

// TrustPolicyFrom возвращает политику из ctx (нулевая — доверие как раньше).
func TrustPolicyFrom(ctx context.Context) TrustPolicy {
	if ctx == nil {
		return TrustPolicy{}
	}
	p, _ := ctx.Value(trustPolicyKey{}).(TrustPolicy)
	return p
}

// AllowUntrustedPlugins — вызов в trusted-коде, который осознанно разрешает
// запуск внешнего кода (он всё равно уходит в изолятор).
func AllowUntrustedPlugins(ctx context.Context) context.Context {
	return WithTrustPolicy(ctx, TrustPolicy{AllowUntrusted: true})
}

// TrustDecision — вердикт ядра по конкретному плагину.
type TrustDecision struct {
	// Trusted — плагину выданы права пользователя (полный exec без песочницы).
	Trusted bool
	// Digest — фактический хэш содержимого каталога плагина; пуст, если
	// содержимое прочитать не удалось. Попадает в диагностику, чтобы оператор
	// видел, ЧТО именно он должен внести в allow-list.
	Digest string
	// Reason — человекочитаемое объяснение для untrusted. Оно же уходит в
	// сообщение об отказе, поэтому обязано называть причину, а не состоять из
	// общих слов: «не доверен» без причины неотличимо от «сломалось».
	Reason string
}

// DecideTrust — единственное место, где решается доверие к коду плагина.
//
// Правило одно и оно инверсивное: ДОВЕРЕНО только тогда, когда id и хэш
// содержимого есть в allow-list оператора. Манифест может только понизить
// доверие:
//
//   - `sandbox: untrusted` в манифесте — всегда untrusted, даже при совпавшем
//     хэше. Автор может знать, что его код опаснее обычного (или репозиторий
//     обновился, а автор забыл), и это нельзя перебить ничем извне;
//   - `sandbox: trusted` в манифесте НЕ даёт доверия вообще: раньше это было
//     единственным источником, и именно поэтому отсутствие строки означало
//     «доверен».
//
// Порядок проверок соответствует направлению ошибки: сомнение всегда даёт
// untrusted. В частности, каталог, который не удалось прочитать, — это
// untrusted, а не «доверимся, не разобрались».
func DecideTrust(m *pipeline.Manifest, policy TrustPolicy) TrustDecision {
	if m == nil {
		return TrustDecision{Reason: "манифест не загружен"}
	}
	// Написанное агентом — внешний код по построению. Это НЕ понижение
	// доверия, а отдельное правило: агент создаёт плагины сам, и доверять
	// результату по его же подписи нельзя в принципе.
	if IsAgentWrittenPlugin(m) {
		return TrustDecision{Reason: "плагин написан агентом (" + AgentPluginDir + "/): внешний код по построению"}
	}
	// Манифест вправе только понизить доверие.
	if m.Sandbox == pipeline.SandboxUntrusted {
		return TrustDecision{Reason: "манифест объявил себя внешним кодом (sandbox: untrusted)"}
	}
	// Точка входа обязана лежать внутри хэшируемого содержимого: иначе плагин
	// исполнял бы файл, которого хэш не видит.
	if !entryIsHashed(m.Dir, m.Runtime.Entry) {
		return TrustDecision{Reason: "runtime.entry " + m.Runtime.Entry + " вне хэшируемого содержимого плагина"}
	}
	digest, err := ContentDigest(m.Dir)
	if err != nil {
		return TrustDecision{Reason: "содержимое плагина не проверяется: " + err.Error()}
	}
	if policy.Trusted.Allows(m.ID, digest) {
		return TrustDecision{Trusted: true, Digest: digest}
	}
	reason := "плагин " + m.ID + " не в allow-list доверия ядра (ожидалась запись " +
		m.ID + "@" + digest + ")"
	if hasBytecode(m.Dir) {
		reason += "; в каталоге плагина есть байткод Python (__pycache__/*.pyc) — он входит в хэш, " +
			"удалите его, если плагин не менялся"
	}
	return TrustDecision{Digest: digest, Reason: reason}
}

// IsTrusted — короткая форма DecideTrust для мест, где нужно только «да/нет».
func IsTrusted(m *pipeline.Manifest, policy TrustPolicy) bool {
	return DecideTrust(m, policy).Trusted
}

// untrustedCodeError — описание отказа, возвращаемое как платформенная ошибка
// протокола (ErrCode "sandbox_unavailable"): ран падает, ничего не запускается.
func untrustedCodeError(m *pipeline.Manifest, reason string) *ExecResult {
	return &ExecResult{
		Platform: true,
		ErrCode:  "sandbox_unavailable",
		ErrMsg: "плагин " + m.ID + " — внешний код (" + reason + "), " +
			"но изоляция недоступна: согласие оператора не получено — запуск невозможен",
		ExitCode: 2,
	}
}

// enforceTrust — fail-closed проверка ДО запуска: без согласия ядра внешний код
// не запускается. Само наличие песочницы проверяется в execPluginEnv: там, где
// изолятора нет, процесс не создаётся (ErrSandboxUnsupported).
func enforceTrust(m *pipeline.Manifest, policy TrustPolicy) *ExecResult {
	if m == nil {
		return nil
	}
	decision := DecideTrust(m, policy)
	switch {
	case policy.DenyUntrusted:
		return untrustedCodeError(m, "ядро запущено с --deny-untrusted-plugins")
	case !decision.Trusted && !policy.AllowUntrusted:
		return untrustedCodeError(m, decision.Reason+
			"; нет согласия на запуск внешнего кода (нужен флаг --allow-untrusted-plugins)")
	}
	return nil
}
