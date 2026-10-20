package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/wykserdex/fidespipe/internal/api"
	"github.com/wykserdex/fidespipe/internal/gate"
	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/mcp"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

// RunMCP — fidespipe mcp --plugins=<dir>... [--workdir=<dir>] [--runs-dir=<dir>]
// [--gui-listen=127.0.0.1:0] [--no-gui] [--no-open] [--print-link].
//
// Stdio JSON-RPC. Изоляция (Фаза 4.2): proto = исходный stdout, os.Stdout →
// stderr, раны Quiet, терминальный гейт запрещён (MCPMode).
//
// Гейты: MCP-процесс поднимает на 127.0.0.1 встроенную консоль с сессией
// человека. Когда ран агента доходит до human_gate, браузер человека
// открывается на этом ране (одноразовый код обмена); агент получает только
// адрес без кода. Решение принимает человек в браузере → source: gui.
func RunMCP(args []string) {
	var plugins []string
	workdir, runsdir := "", ""
	guiListen := "127.0.0.1:0"
	noGUI, noOpen, printLink := false, false, false
	// Политика доверия MCP-сервера. Всё по умолчанию закрыто: без
	// --allow-agent-exec инструмент exec_plugin отказывает, без
	// --allow-untrusted-plugins не запускается внешний код.
	allowAgentExec, allowUntrusted, denyUntrusted := false, false, false
	// Операторский обход требования одобрения. По умолчанию выключен: ран,
	// где опасный шаг (сеть/диск/секреты) идёт без human_gate, отклоняется с
	// E_GATE_REQUIRED. Локальное доверенное использование.
	allowUngatedRuns := false
	// Гейты через elicitation клиента. Отдельный флаг, а не «включено, если
	// клиент умеет»: поддержку клиента видно из initialize, но доверять ей —
	// решение оператора (см. комментарий в mcp.Options.GateElicitation).
	gateElicitation := false
	trustCfg := ""
	// docs/mcp.md и конфиги клиентов пишут "--plugins /abs" (через пробел):
	// нормализуем в "--plugins=/abs" до разбора.
	args = joinFlagValues(args, "--plugins", "--plugin", "--workdir", "--runs-dir", "--gui-listen")
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--plugins="):
			plugins = append(plugins, strings.TrimPrefix(a, "--plugins="))
		case strings.HasPrefix(a, "--plugin="):
			plugins = append(plugins, strings.TrimPrefix(a, "--plugin="))
		case strings.HasPrefix(a, "--workdir="):
			workdir = strings.TrimPrefix(a, "--workdir=")
		case strings.HasPrefix(a, "--runs-dir="):
			runsdir = strings.TrimPrefix(a, "--runs-dir=")
		case strings.HasPrefix(a, "--gui-listen="):
			guiListen = strings.TrimPrefix(a, "--gui-listen=")
		case a == "--no-gui":
			noGUI = true
		case a == "--no-open":
			noOpen = true
		case a == "--print-link":
			printLink = true
		case a == "--allow-agent-exec":
			allowAgentExec = true
		case a == "--allow-untrusted-plugins":
			allowUntrusted = true
		case a == "--deny-untrusted-plugins":
			denyUntrusted = true
		case a == "--allow-unapproved-runs":
			allowUngatedRuns = true
		case a == "--gate-elicitation":
			gateElicitation = true
		case strings.HasPrefix(a, "--trust-config="):
			trustCfg = strings.TrimPrefix(a, "--trust-config=")
		case a == "--help" || a == "-h":
			fmt.Fprintln(os.Stderr, "fidespipe mcp --plugins=<dir> [--plugins=<dir>...] [--workdir=<dir>] [--runs-dir=<dir>]")
			fmt.Fprintln(os.Stderr, "          [--gui-listen=127.0.0.1:0] [--no-gui] [--no-open] [--print-link]")
			fmt.Fprintln(os.Stderr, "          [--allow-agent-exec] [--allow-untrusted-plugins] [--deny-untrusted-plugins]")
			fmt.Fprintln(os.Stderr, "          [--allow-unapproved-runs] [--gate-elicitation]")
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "exec_plugin (запуск плагинов агентом) по умолчанию ЗАПРЕЩЁН.")
			fmt.Fprintln(os.Stderr, "Включается только --allow-agent-exec; каждый запуск пишется в <runs-dir>/agent-exec.jsonl.")
			fmt.Fprintln(os.Stderr, "Плагин из agent-plugins/ всегда считается внешним кодом: для него нужен --allow-untrusted-plugins.")
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "run_pipeline по умолчанию ТРЕБУЕТ human_gate перед первым опасным шагом")
			fmt.Fprintln(os.Stderr, "(сеть, запись на диск, чтение секретов — по capabilities плагина).")
			fmt.Fprintln(os.Stderr, "--allow-unapproved-runs снимает это требование; каждый обход пишется в <runs-dir>/gate-bypass.jsonl.")
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "--gate-elicitation ведёт гейты через диалог MCP-клиента (elicitation/create, протокол 2025-06-18),")
			fmt.Fprintln(os.Stderr, "если клиент объявил эту возможность. По умолчанию выключено: fidespipe не может проверить, что за")
			fmt.Fprintln(os.Stderr, "клиентом сидит человек, поэтому решение человека называет таковым только оператор, включивший флаг.")
			return
		default:
			fmt.Fprintf(os.Stderr, "неизвестный аргумент %q (см. --help)\n", a)
			os.Exit(2)
		}
	}
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	absWork, _ := filepath.Abs(workdir)
	if runsdir == "" {
		runsdir = journal.DefaultRunsDirAt(absWork)
	}

	// Изоляция stdout: JSON-RPC — только в исходный stdout, всё остальное — в stderr.
	proto := os.Stdout
	os.Stdout = os.Stderr

	// Доверие: встроенный allow-list (пины реестра) + конфиг оператора. Без
	// него exec_plugin уводил бы в песочницу даже официальные плагины, а на
	// хостах без изолятора отказывал бы вовсе.
	trusted, err := plugin.EffectiveAllowList(trustConfigPath(trustCfg))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp: ошибка конфига доверия:", err)
		os.Exit(2)
	}

	opts := mcp.Options{
		PluginsDirs: plugins,
		WorkDir:     absWork,
		RunsDir:     runsdir,
		Trust: plugin.TrustPolicy{
			AgentCanExec:         allowAgentExec,
			AllowUntrusted:       allowUntrusted,
			DenyUntrusted:        denyUntrusted,
			Trusted:              trusted,
			AgentCanWritePlugins: false, // запись плагинов агентом не реализована
		},
		AllowUngatedRuns: allowUngatedRuns,
		GateElicitation:  gateElicitation,
	}
	if allowUngatedRuns {
		// Обход виден в логе сразу при старте, а не только в момент обхода:
		// оператор должен видеть, что сервер запущен с ослабленным требованием.
		fmt.Fprintln(os.Stderr, "fidespipe mcp: ВНИМАНИЕ --allow-unapproved-runs: ран с опасным шагом (сеть/диск/секреты) пойдёт без human_gate. Каждый обход пишется в", filepath.Join(runsdir, "gate-bypass.jsonl"))
	}
	if !noGUI {
		h, err := startHumanConsole(guiListen, plugins, absWork, runsdir, !noOpen, printLink)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp: консоль для гейтов не поднялась:", err, "(гейты будут недоступны)")
		} else {
			opts.Human = h
		}
	}
	srv, err := mcp.NewServer(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		os.Exit(1)
	}
	t := mcp.NewTransport(os.Stdin, proto)
	if err := srv.Serve(t); err != nil {
		if !strings.Contains(err.Error(), "EOF") {
			fmt.Fprintln(os.Stderr, "mcp:", err)
		}
	}
}

// humanConsole — mcp.HumanChannel поверх встроенного api.Server.
type humanConsole struct {
	srv   *api.Server
	base  string // http://127.0.0.1:PORT (без кода)
	code  string // текущий одноразовый код обмена
	open  bool
	print bool
	mu    sync.Mutex
}

func startHumanConsole(listen string, plugins []string, workdir, runsDir string, open, printLink bool) (*humanConsole, error) {
	pluginsDir := filepath.Join(workdir, "plugins")
	if len(plugins) > 0 {
		pluginsDir = plugins[0]
		if !filepath.IsAbs(pluginsDir) {
			pluginsDir = filepath.Join(workdir, pluginsDir)
		}
	}
	if !filepath.IsAbs(runsDir) {
		runsDir = filepath.Join(workdir, runsDir)
	}
	// Тот же запрет, что у `fidespipe gui`: консоль гейтов на внешнем адресе —
	// это чужой доступ к решениям человека, и по умолчанию он не включается.
	if err := api.ValidateListen(api.ListenOptions{Addr: listen}); err != nil {
		return nil, err
	}
	srv := api.NewServer(pluginsDir, filepath.Join(workdir, "examples"), runsDir)
	code, err := api.NewPairingCode()
	if err != nil {
		return nil, err
	}
	srv.EnableSession(code)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	if _, port, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		if err := srv.ConfigureListen(api.ListenOptions{Addr: listen, ResolvedPort: port}); err != nil {
			return nil, err
		}
	}
	go func() { _ = srv.HTTPServer(ln.Addr().String()).Serve(ln) }()
	base := "http://" + ln.Addr().String()
	fmt.Fprintf(os.Stderr, "fidespipe mcp: консоль гейтов %s (вход — по одноразовому коду, только в браузер человека)\n", base)
	return &humanConsole{srv: srv, base: base, code: code, open: open, print: printLink}, nil
}

func (h *humanConsole) AttachRun(id string, ui *gate.ChannelUI, cancel context.CancelFunc) {
	h.srv.AttachRun(id, ui, cancel)
}

func (h *humanConsole) DetachRun(id string) { h.srv.DetachRun(id) }

func (h *humanConsole) PublicURL(id string) string { return h.base + "/?run=" + id }

// GateWaiting — открыть браузер человека на этом ране. Код одноразовый, поэтому
// на каждое ожидание гейта выдаётся свежий: иначе второй гейт (или вторая
// вкладка) остался бы без входа. Агент через PublicURL кода не получает.
func (h *humanConsole) GateWaiting(id string) {
	h.mu.Lock()
	if fresh, err := api.NewPairingCode(); err == nil {
		h.code = fresh
		h.srv.RotatePairingCode(fresh)
	}
	code := h.code
	link := h.base + "/?run=" + id + "&c=" + code
	h.mu.Unlock()

	if h.print {
		fmt.Fprintln(os.Stderr, "fidespipe mcp: гейт ждёт человека. Откройте в браузере:", link)
		fmt.Fprintln(os.Stderr, "            или введите код на странице GUI:", code)
	}
	if !h.open {
		if !h.print {
			fmt.Fprintln(os.Stderr, "fidespipe mcp: гейт ждёт человека (--no-open без --print-link: ссылку никто не увидит)")
		}
		return
	}
	openBrowser(link)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Start()
}

// joinFlagValues: ["--x", "v"] → ["--x=v"] для перечисленных флагов.
func joinFlagValues(args []string, flags ...string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		matched := false
		for _, f := range flags {
			if a == f && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				out = append(out, f+"="+args[i+1])
				i++
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, a)
		}
	}
	return out
}
