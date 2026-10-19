package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wykserdex/wedra/internal/common"
	"github.com/wykserdex/wedra/internal/pipeline"
)

type Manifest = pipeline.Manifest

type ExecResult struct {
	Output    map[string]interface{}
	ErrCode   string
	ErrMsg    string
	Retryable bool
	Platform  bool
	ExitCode  int
	Stderr    string
	Duration  time.Duration
	// v0.9: Cancelled — процесс убит внешней отменой (ctx рана), не
	// таймаутом шага. ErrCode при этом "cancelled", Platform=true.
	Cancelled bool
}

func (r *ExecResult) OK() bool { return !r.Platform && r.ExitCode == 0 && r.ErrCode == "" }

// ShouldRetry — PROTOCOL §3/§6: retry повторяет таймауты и доменные ошибки
// с retryable: true. Всё остальное — сразу на политику шага. Платформенная
// ошибка (exit>=2, таймаут, невалидный stdout) останавливает ран всегда и
// политикой не переопределяется, поэтому просьба плагина retryable: true на
// exit>=2 ран не ретраит (иначе плагин мог бы заставить ядро бесконечно
// перезапускать то, что стопит по протоколу).
func (r *ExecResult) ShouldRetry() bool {
	if r.Cancelled {
		return false
	}
	if r.ErrCode == "timeout" {
		return true
	}
	if r.Platform {
		return false
	}
	return r.Retryable
}

// PlatformErrCode — код платформенной ошибки в форме `platform:<code>`
// (PROTOCOL §3, ERRORS.md). Префикс добавляется ровно один раз: плагин,
// приславший код, уже с `platform:`, не должен размножать его до
// `platform:platform:<code>`.
func PlatformErrCode(code string) string {
	if code == "" {
		return ""
	}
	if strings.HasPrefix(code, "platform:") {
		return code
	}
	return "platform:" + code
}

type wireResponse struct {
	Status string                 `json:"status"`
	Output map[string]interface{} `json:"output"`
	Error  *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

var (
	pythonCacheMu      sync.Mutex
	pythonCachePathEnv string
	pythonCacheBin     string
)

func pythonInterpreter() (string, error) {
	pathEnv := os.Getenv("PATH")
	pythonCacheMu.Lock()
	if pythonCacheBin != "" && pythonCachePathEnv == pathEnv {
		if info, err := os.Stat(pythonCacheBin); err == nil && !info.IsDir() {
			cached := pythonCacheBin
			pythonCacheMu.Unlock()
			return cached, nil
		}
	}
	pythonCacheMu.Unlock()

	var firstFound string
	for _, name := range []string{"python3", "python"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if firstFound == "" {
			firstFound = p
		}
		if runtime.GOOS != "windows" {
			pythonCacheMu.Lock()
			pythonCachePathEnv = pathEnv
			pythonCacheBin = p
			pythonCacheMu.Unlock()
			return p, nil
		}
		// Проба на Windows обязательна, и её провал — это ответ, а не формальность.
		// В PATH очень часто лежит заглушка Microsoft Store
		// (WindowsApps\python3.exe): она печатает «Python was not found but can be
		// installed from the Microsoft Store» и выходит с 9009. Раньше мы всё равно
		// возвращали этот путь, и каждый запуск плагина падал с 9009 и внятным
		// только на первый взгляд сообщением. Теперь непроверенный кандидат
		// пропускается, и следующий настоящий интерпретатор находится нормально.
		probe := exec.Command(p, "-X", "utf8", "-c", "import sys; print(sys.executable)")
		out, probeErr := common.Output(probe)
		if probeErr != nil {
			continue
		}
		candidate := strings.TrimSpace(string(out))
		resolved := p
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			resolved = candidate
		}
		pythonCacheMu.Lock()
		pythonCachePathEnv = pathEnv
		pythonCacheBin = resolved
		pythonCacheMu.Unlock()
		return resolved, nil
	}
	if firstFound != "" {
		return "", fmt.Errorf("найден кандидат python (%s), но он не проходит пробу — переустановите интерпретатор или уберите заглушку Microsoft Store из PATH", firstFound)
	}
	return "", fmt.Errorf("не найден интерпретатор python (python3/python)")
}

func markContextResult(parent, ctx context.Context, res *ExecResult) bool {
	if parent.Err() != nil {
		res.Platform, res.Cancelled, res.ErrCode, res.ErrMsg = true, true, "cancelled", "ран отменён"
		res.ExitCode = 2
		return true
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Platform, res.ErrCode, res.ErrMsg = true, "timeout", "плагин превысил таймаут"
		res.ExitCode = 2
		return true
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		res.Platform, res.Cancelled, res.ErrCode, res.ErrMsg = true, true, "cancelled", "плагин отменён"
		res.ExitCode = 2
		return true
	}
	return false
}

func Exec(m *Manifest, inputJSON []byte, timeout time.Duration) *ExecResult {
	return ExecWithEnv(m, inputJSON, timeout, nil)
}

func ExecWithEnv(m *Manifest, inputJSON []byte, timeout time.Duration, extraEnv []string) *ExecResult {
	return execPluginEnv(context.Background(), m, inputJSON, timeout, extraEnv)
}

// ExecWithEnvCtx — v0.9: как ExecWithEnv, но с родительским ctx (отмена рана).
// Отмена parent убивает группу процессов плагина; результат — ErrCode
// "cancelled", Cancelled=true (не "timeout" и не retryable).
func ExecWithEnvCtx(parent context.Context, m *Manifest, inputJSON []byte, timeout time.Duration, extraEnv []string) *ExecResult {
	if parent == nil {
		parent = context.Background()
	}
	return execPluginEnv(parent, m, inputJSON, timeout, extraEnv)
}

func execPluginEnv(parent context.Context, m *Manifest, input []byte, timeout time.Duration, extraEnv []string) *ExecResult {
	res := &ExecResult{ExitCode: -1}
	if err := parent.Err(); err != nil {
		res.Platform, res.Cancelled, res.ErrCode, res.ErrMsg = true, true, "cancelled", "ран отменён до запуска шага"
		res.ExitCode = 2
		return res
	}
	// Fail-closed: внешний код не запускается без изоляции (process.go —
	// единственная точка создания plugin-процесса, включая MCP/transport).
	trustPolicy := TrustPolicyFrom(parent)
	if denied := enforceTrust(m, trustPolicy); denied != nil {
		return denied
	}
	// Решение о доверии выносится ДО сборки команды и используется дальше для
	// выбора способа запуска. Два независимых вызова DecideTrust разошлись бы
	// только при гонке с подменой файла между ними, а расхождение «проверили как
	// доверенный, запустили в песочнице» (или наоборот) — это ровно тот класс
	// ошибок, который здесь и ловится.
	trusted := IsTrusted(m, trustPolicy)

	var argv []string
	switch m.Runtime.Type {
	case "python":
		py, err := pythonInterpreter()
		if err != nil {
			res.Platform, res.ErrCode, res.ErrMsg = true, "runtime_missing", err.Error()
			res.ExitCode = 2
			return res
		}
		entry, err := filepath.Abs(filepath.Join(m.Dir, m.Runtime.Entry))
		if err != nil {
			res.Platform, res.ErrCode, res.ErrMsg = true, "spawn_failed", err.Error()
			res.ExitCode = 2
			return res
		}
		argv = []string{py, entry}
	case "binary":
		entry, err := filepath.Abs(filepath.Join(m.Dir, m.Runtime.Entry))
		if err != nil {
			res.Platform, res.ErrCode, res.ErrMsg = true, "spawn_failed", err.Error()
			res.ExitCode = 2
			return res
		}
		argv = []string{entry}
	default:
		res.Platform, res.ErrCode, res.ErrMsg = true, "runtime_unknown", "runtime.type: "+m.Runtime.Type
		res.ExitCode = 2
		return res
	}

	// Таймаут запускается ПОСЛЕ поиска интерпретатора (на Windows первый вызов
	// pythonInterpreter делает пробу через подпроцесс, и это время не должно
	// съедать бюджет самого плагина).
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// Внешний код запускается только внутри изолятора; если изолятор на хосте
	// недоступна — отказ до создания процесса (fail-closed).
	var cmd *exec.Cmd
	var netSetup sandboxNetwork
	scratch := ""
	if !trusted {
		var cleanup func()
		var err error
		scratch, cleanup, err = newSandboxScratch()
		if err == nil {
			cmd, netSetup, err = sandboxCommand(ctx, argv, m, scratch)
		}
		if err != nil {
			if cleanup != nil {
				cleanup()
			}
			res.Platform, res.ErrCode, res.ErrMsg = true, "sandbox_unavailable", err.Error()
			res.ExitCode = 2
			return res
		}
		defer cleanup()
	} else {
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
	}

	cmd.Dir = m.Dir
	cmd.Stdin = bytes.NewReader(input)
	baseEnv := pluginBaseEnv(m)
	if !trusted {
		// В песочнице плагину не нужны профиль пользователя и его каталоги:
		// HOME/USERPROFILE/APPDATA вырезаются, секреты не передаются вовсе.
		// HOME/TMPDIR указывают на приватный scratch этого запуска.
		baseEnv = append(untrustedBaseEnv(m), sandboxScratchEnv(scratch)...)
		// Egress-прокси поднимается только когда плагину достаётся сеть,
		// которой он может дотянуться. Изолированная песочница до хоста не
		// дотягивается вовсе (проверено: и loopback хоста, и шлюз
		// userspace-стека изнутри недоступны), поэтому прокси там бесполезен,
		// а HTTP_PROXY с адресом хоста только вводил бы честный плагин в
		// заблуждение — он получал бы отказ на каждый запрос.
		//
		// Прокси остаётся в коде для будущего бэкенда с общим netns; на
		// текущих платформах он не вызывается, и это не должно читаться как
		// «фильтр работает».
		if declaresNetwork(m) && netSetup == nil {
			env, stop, err := sandboxEgress(ctx, m)
			if err != nil {
				res.Platform, res.ErrCode, res.ErrMsg = true, "sandbox_unavailable", err.Error()
				res.ExitCode = 2
				return res
			}
			defer stop()
			baseEnv = append(baseEnv, env...)
		}
	}
	if m.Runtime.Type == "python" {
		// PYTHONDONTWRITEBYTECODE: интерпретатор не создаёт __pycache__ внутри
		// каталога плагина. Байткод входит в хэш содержимого (trustlist.go), и
		// созданный при запуске кэш менял бы хэш доверенного плагина.
		baseEnv = append(baseEnv, "PYTHONUTF8=1", "PYTHONDONTWRITEBYTECODE=1")
	}
	cmd.Env = mergeEnv(baseEnv, extraEnv)
	// v0.23: свой process group — таймаут убивает группу, а не только прямой
	// процесс (python-плагин с дочерними больше не оставляет сирот).
	// Убийство группы — в момент таймаута (goroutine): Wait ждёт закрытия
	// пайпов, унаследованных дочерними, и без group kill «замрёт» до их
	// естественной смерти (sleep 30 = 30 секунд).
	//
	// common.ProcWaitDelay ограничивает ровно эту ждобу: сколько готовы ждать
	// закрытия пайпов вывода ПОСЛЕ выхода процесса. Без него cmd.Wait() ждёт
	// вечно, если пайп удерживает кто-то живой (на Windows потомок наследует
	// stdin/stdout/stderr всегда — см. golang/go#60942). Величина не влияет на
	// нормальные плагины: они пишут ответ до выхода, и WaitDelay не
	// добавляет им задержки — платит только тот, кто действительно утек.
	cmd.WaitDelay = common.ProcWaitDelay
	if err := prepareProcessGroup(cmd); err != nil {
		res.Platform, res.ErrCode, res.ErrMsg = true, "process_group", err.Error()
		res.ExitCode = 2
		return res
	}
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
	// v0.23: stdout/stderr с лимитом (гигантский вывод = не вся память процесса)
	stdoutCap, stderrCap := 16<<20, 1<<20
	stdout := &cappedWriter{buf: &bytes.Buffer{}, limit: stdoutCap}
	stderr := &cappedWriter{buf: &bytes.Buffer{}, limit: stderrCap}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Start отдельно от Wait: после Start() cmd.Process уже установлена,
	// и гоорутина group-kill читает её без data race (проверено -race).
	if serr := cmd.Start(); serr != nil {
		cleanupProcessGroup(cmd)
		if markContextResult(parent, ctx, res) {
			return res
		}
		res.Platform, res.ErrCode, res.ErrMsg = true, "spawn_failed", serr.Error()
		res.ExitCode = 2
		return res
	}
	if aerr := attachProcessGroup(cmd); aerr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cleanupProcessGroup(cmd)
		if markContextResult(parent, ctx, res) {
			return res
		}
		res.Platform, res.ErrCode, res.ErrMsg = true, "process_group", aerr.Error()
		res.ExitCode = 2
		return res
	}
	defer cleanupProcessGroup(cmd)
	go func() {
		<-ctx.Done()
		killProcessGroup(cmd)
	}()

	// Сеть поднимается ПОСЛЕ старта процесса и ДО его фактического выполнения:
	// плагин запущен, но заперт на воротах, пока tap0 не поднят. Ошибка здесь —
	// отказ, а не запуск без запрошенной сети.
	if netSetup != nil {
		release, aerr := netSetup.attach(cmd)
		if aerr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			cleanupProcessGroup(cmd)
			if markContextResult(parent, ctx, res) {
				return res
			}
			// stderr изолятора — единственное, что объяснит отказ: сам bwrap
			// молчит и в журнал не пишет.
			if s := strings.TrimSpace(stderr.String()); s != "" {
				aerr = fmt.Errorf("%w (изолятор: %s)", aerr, s)
			}
			res.Platform, res.ErrCode, res.ErrMsg = true, "sandbox_unavailable", aerr.Error()
			res.ExitCode = 2
			return res
		}
		defer release()
	}

	start := time.Now()
	runErr := cmd.Wait()
	res.Duration = time.Since(start)
	res.Stderr = common.Truncate(stderr.String(), 4000)

	// отмена родителя проверяется первой: ctx.Err() дочернего при отмене
	// родителя — Canceled, при истечении своего таймаута — DeadlineExceeded
	if parent.Err() != nil {
		res.Platform, res.Cancelled = true, true
		res.ErrCode, res.ErrMsg = "cancelled", "ран отменён — процесс плагина остановлен"
		res.ExitCode = 2
		return res
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.Platform = true
		res.ErrCode, res.ErrMsg = "timeout", "плагин превысил таймаут "+timeout.String()
		res.ExitCode = 2
		return res
	}
	if runErr != nil {
		var ee *exec.ExitError
		switch {
		case errors.As(runErr, &ee):
			res.ExitCode = ee.ExitCode()
		case errors.Is(runErr, exec.ErrWaitDelay):
			// Процесс отработал и вышел, но кто-то ещё держит его пайп
			// вывода: почти всегда это фоновый потомок, который плагин запустил
			// и забыл. Раньше cmd.Wait() ждал закрытия пайпа БЕСКОНЕЧНО, и
			// успешный плагин с фоновой задачей приводил ран к таймауту.
			//
			// Это НЕ ошибка плагина: его ответ уже получен, поэтому идём
			// успешной веткой, но след утечки оставляем в stderr — иначе
			// «успех» выглядит как обычный ран и молча скрывает оставшегося
			// жить потомка.
			res.ExitCode = 0
			if cmd.ProcessState != nil {
				res.ExitCode = cmd.ProcessState.ExitCode()
			}
			warn := "плагин оставил фоновый процесс: пайп вывода не закрылся за " +
				common.ProcWaitDelay.String() + ", ответ плагина принят, потомок мог остаться жив"
			if res.Stderr == "" {
				res.Stderr = warn
			} else {
				res.Stderr = common.Truncate(res.Stderr, 4000) + "\n[wedra] " + warn
			}
		default:
			res.Platform, res.ErrCode, res.ErrMsg = true, "spawn_failed", runErr.Error()
			res.ExitCode = 2
			return res
		}
	} else {
		res.ExitCode = 0
	}

	if stdout.overflow {
		res.Platform = true
		res.ErrCode, res.ErrMsg = "protocol_violation", fmt.Sprintf("stdout плагина превысил лимит %d МБ — обрезан (возможно, мусор в выводе)", stdoutCap>>20)
		res.ExitCode = 2
		return res
	}
	var wr wireResponse
	outTrim := bytes.TrimSpace(stdout.Bytes())
	if len(outTrim) > 0 {
		if err := json.Unmarshal(outTrim, &wr); err != nil {
			res.Platform, res.ErrCode = true, "protocol_violation"
			res.ErrMsg = fmt.Sprintf("на stdout не JSON по протоколу (exit %d): %s", res.ExitCode, common.Truncate(string(outTrim), 200))
			if res.ExitCode == 0 {
				res.ExitCode = 2
			}
			return res
		}
	}

	switch {
	case res.ExitCode == 0:
		if wr.Status != "ok" {
			res.Platform, res.ErrCode = true, "protocol_violation"
			res.ErrMsg = "exit 0, но status != ok"
			res.ExitCode = 2
			return res
		}
		res.Output = wr.Output
		if res.Output == nil {
			res.Output = map[string]interface{}{}
		}
	case res.ExitCode == 1:
		res.ErrCode, res.ErrMsg = "plugin_error", "доменная ошибка без описания"
		if wr.Error != nil {
			res.ErrCode = wr.Error.Code
			res.ErrMsg = wr.Error.Message
			res.Retryable = wr.Error.Retryable
		}
	default:
		res.Platform = true
		if wr.Error != nil && wr.Error.Code != "" {
			// PROTOCOL §3: на exit>=2 error.code сохраняется как
			// platform:<code> (fallback — crash). Retryable из конверта
			// пишется в step_end для триажа, но ран не ретраит:
			// ShouldRetry() для платформенных ошибок = false.
			res.ErrCode = PlatformErrCode(wr.Error.Code)
			res.ErrMsg = wr.Error.Message
			if res.ErrMsg == "" {
				res.ErrMsg = fmt.Sprintf("exit %d: %s", res.ExitCode, common.Truncate(string(outTrim), 200))
			}
			res.Retryable = wr.Error.Retryable
		} else {
			res.ErrCode, res.ErrMsg = "crash", fmt.Sprintf("exit %d: %s", res.ExitCode, common.Truncate(string(outTrim), 200))
		}
	}
	return res
}

var pluginSystemEnv = map[string]struct{}{
	"PATH": {}, "PATHEXT": {}, "SYSTEMROOT": {}, "WINDIR": {}, "COMSPEC": {},
	"TEMP": {}, "TMP": {}, "TMPDIR": {}, "LANG": {}, "LC_ALL": {}, "LC_CTYPE": {},
	"TZ": {}, "HOME": {}, "USERPROFILE": {}, "APPDATA": {}, "LOCALAPPDATA": {},
	"PROGRAMDATA": {}, "PROGRAMFILES": {}, "PROGRAMFILES(X86)": {},
	"COMMONPROGRAMFILES": {}, "COMMONPROGRAMFILES(X86)": {}, "COMMONPROGRAMW6432": {},
	"PROGRAMW6432": {}, "SYSTEMDRIVE": {}, "HOMEDRIVE": {}, "HOMEPATH": {},
	"PUBLIC": {}, "USERNAME": {}, "USERDOMAIN": {}, "SESSIONNAME": {},
	"PROCESSOR_ARCHITECTURE": {}, "NUMBER_OF_PROCESSORS": {}, "OS": {}, "PSMODULEPATH": {},
}

func pluginBaseEnv(m *Manifest) []string {
	return baseEnvFor(m, pluginSystemEnv, true)
}

// untrustedSystemEnv — минимальный набор для изолированного плагина. Профиль
// пользователя (HOME/USERPROFILE/APPDATA/LOCALAPPDATA/PROGRAMDATA/PUBLIC) и
// доменные переменные вырезаны: иначе песочница протекает в файлы, которые
// плагин не должен видеть.
var untrustedSystemEnv = map[string]struct{}{
	"PATH": {}, "PATHEXT": {}, "SYSTEMROOT": {}, "WINDIR": {}, "COMSPEC": {},
	"TEMP": {}, "TMP": {}, "TMPDIR": {}, "LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "TZ": {},
}

// untrustedBaseEnv — окружение внешнего кода: только системные переменные из
// узкого allowlist, никаких permissions.secrets (их валидатор и не пропустит).
func untrustedBaseEnv(m *Manifest) []string {
	return baseEnvFor(m, untrustedSystemEnv, false)
}

func baseEnvFor(m *Manifest, allow map[string]struct{}, withSecrets bool) []string {
	out := make([]string, 0, len(allow))
	// Переменная донора: путь к поставленному исполняемому файлу.
	//
	// Имя переменной объявляет сам плагин (donor.bin_env), потому что у каждого
	// оно своё — EXIFTOOL_BIN, GOSPIDER_BIN, JOOMSCAN_BIN — и ядро не может
	// угадать. Путь абсолютный: у изолированного плагина PATH вырезан
	// нарочно (чтобы песочница не протекала), и имя из PATH не нашлось бы.
	if v := donorEnvFor(m); v != "" {
		out = append(out, v)
	}
	for _, kv := range os.Environ() {
		i := indexByte(kv, '=')
		if i <= 0 || !validEnvName(kv[:i]) {
			continue
		}
		if _, ok := allow[strings.ToUpper(kv[:i])]; ok {
			out = append(out, kv)
		}
	}
	if !withSecrets || m == nil {
		return out
	}
	for _, name := range m.Permissions.Secrets {
		if !validEnvName(name) {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}

func validEnvName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "=\x00")
}

func mergeEnv(base, extra []string) []string {
	m := map[string]string{}
	for _, kv := range base {
		if i := indexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	for _, kv := range extra {
		if i := indexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func EnforceOutput(m *Manifest, out map[string]interface{}) (clean map[string]interface{}, dropped []string, err error) {
	clean = map[string]interface{}{}
	for k, v := range out {
		if _, declared := m.Output[k]; declared {
			clean[k] = v
		} else {
			dropped = append(dropped, k)
		}
	}
	for name, port := range m.Output {
		if _, ok := clean[name]; !ok && !port.Optional {
			return nil, dropped, fmt.Errorf("нарушение контракта: плагин %s не вернул обязательное поле %q", m.ID, name)
		}
		// v0.23: контракт = есть И правильного типа/формата (README: «ядро
		// проверяет после каждого запуска»)
		if v, ok := clean[name]; ok {
			if err := CheckValue(name, fmt.Sprintf("вывод %s", m.ID), port, v); err != nil {
				return nil, dropped, err
			}
		}
	}
	return clean, dropped, nil
}

// cappedWriter — буфер с лимитом (v0.23: защита от гигантского вывода).
// NB: НЕ embed-ит bytes.Buffer: в exec-пути (os/exec → io.Copy → fast path)
// embedded-буфер заполняется, минуя метод-обёртку (проверено экспериментом) —
// лимит молча не срабатывал. Частное поле + свой Write — работает.
type cappedWriter struct {
	buf      *bytes.Buffer
	limit    int
	overflow bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.limit <= 0 {
		return c.buf.Write(p)
	}
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		c.overflow = true
		return len(p), nil
	}
	if len(p) > remaining {
		if _, err := c.buf.Write(p[:remaining]); err != nil {
			return 0, err
		}
		c.overflow = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedWriter) Bytes() []byte  { return c.buf.Bytes() }
func (c *cappedWriter) String() string { return c.buf.String() }
