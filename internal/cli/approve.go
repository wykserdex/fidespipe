package cli

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wykserdex/fidespipe/internal/gate"
	"github.com/wykserdex/fidespipe/internal/journal"
)

// isTTY — интерактивный терминал? Shell-инструменты агентов обычно без TTY.
func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// shortCode — короткий код для подтверждения (4 цифры).
func shortCode() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	n := int(b[0])<<8 | int(b[1])
	return fmt.Sprintf("%04d", n%10000)
}

// RunApprove — fidespipe approve <run> <step> [--server=http://127.0.0.1:8765].
// Запасной вариант без браузера: только интерактивный TTY + короткий код.
// Shell-инструменты агентов обычно без TTY — само-аппрув через них закрыт.
func RunApprove(args []string) {
	var pos []string
	server := "http://127.0.0.1:8765"
	runsDir := journal.DefaultRunsDir()
	linkOnly := false
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--server="):
			server = strings.TrimPrefix(a, "--server=")
		case strings.HasPrefix(a, "--runs-dir="):
			runsDir = strings.TrimPrefix(a, "--runs-dir=")
		case a == "--link":
			linkOnly = true
		case !strings.HasPrefix(a, "-"):
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		fmt.Println("нужно: fidespipe approve <run_id> <step_id> [--server=http://127.0.0.1:8765] [--link]")
		fmt.Println("одобрение — только человеком; --link печатает подписанную ссылку, решение принимается по ней")
		os.Exit(2)
	}
	runID, stepID := pos[0], pos[1]
	if linkOnly {
		// Ссылка — не решение, а право решить. Печатать её можно и без
		// терминала: на безголовом сервере это единственный путь, потому что
		// сам гейт ждёт человека, а терминала у него нет.
		printApproveLink(runID, stepID, server, runsDir)
		return
	}
	if !isTTY() {
		fmt.Println("отказ: approve работает только в интерактивном TTY (stdin не терминал)")
		fmt.Println("попросите пользователя одобрить шаг", stepID, "в окне fidespipe gui")
		os.Exit(1)
	}
	code := shortCode()
	fmt.Printf("подтвердите одобрение run %s шаг %s\n", runID, stepID)
	fmt.Printf("введите код %s: ", code)
	var input string
	_, _ = fmt.Scanln(&input)
	if strings.TrimSpace(input) != code {
		fmt.Println("код не совпал — отказ")
		os.Exit(1)
	}
	fmt.Printf("код принят. Отправьте решение человеком через GUI (%s) или API с сессией:\n", server)
	fmt.Printf("  POST %s/api/runs/%s/gate  {\"action\":\"accept\"}\n", server, runID)
	fmt.Println("  cookie сессии: откройте адрес GUI и введите одноразовый код из терминала fidespipe")
}

// printApproveLink — напечатать подписанную ссылку одобрения.
//
// Путь для БЕЗГОЛОВОГО сервера: оператор заходит по SSH, печатает ссылку и
// открывает её с телефона. Терминал не нужен — печать ссылки не является
// решением, решение принимает человек, открывший ссылку.
//
// Ключ подписи общий с сервером (<runs-dir>/approve.key), поэтому сервер
// проверяет выпущенную здесь ссылку и доставляет решение своему гейту.
func printApproveLink(runID, stepID, server, runsDir string) {
	signer, err := gate.LoadSigner(filepath.Join(runsDir, "approve.key"))
	if err != nil {
		fmt.Println("approve: ключ подписи:", err)
		os.Exit(2)
	}
	link, err := gate.SignLink(signer, server, runID, stepID, time.Hour)
	if err != nil {
		fmt.Println("approve:", err)
		os.Exit(2)
	}
	fmt.Println("ссылка одобрения (открыть с любого устройства, час, работает один раз):")
	fmt.Println(link)
}
