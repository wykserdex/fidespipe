// Command fidespipebot — платный продукт поверх движка: Telegram-бот,
// который продаёт досье по нику/email, собранные настоящими прогонами
// fidespipe. Движок вызывается как CLI, ядро не изменено.
//
//	fidespipebot -config product/bot.config.json          # бот
//	fidespipebot report -run <runDir> -kind username -target x -out <dir>
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/wykserdex/fidespipe/internal/bot"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "report" {
		reportCmd(os.Args[2:])
		return
	}
	cfgPath := flag.String("config", "product/bot.config.json", "путь к конфигу бота")
	flag.Parse()
	cfg, err := bot.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	b, err := bot.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("fidespipebot: repo=%s data=%s цена=%s USDT", cfg.RepoRoot, cfg.DataDir, cfg.PriceUSDT)
	if err := b.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

// reportCmd собирает отчёт из уже существующего прогона: ручная повторная
// выдача, проверка вёрстки и тест без Telegram и оплаты.
func reportCmd(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	runDir := fs.String("run", "", "каталог прогона (с context.json и journal.jsonl)")
	kind := fs.String("kind", "username", "username | email")
	target := fs.String("target", "", "ник или адрес, как в заказе")
	out := fs.String("out", "", "куда писать report.html, evidence.json, journal.jsonl")
	_ = fs.Parse(args)
	if *runDir == "" || *out == "" || *target == "" {
		fmt.Println("нужны -run, -out и -target (и -kind email для адреса)")
		os.Exit(2)
	}
	d, err := bot.ReadDossier(*runDir, *kind, *target)
	if err != nil {
		log.Fatal(err)
	}
	files, err := bot.WriteReport(d, *runDir, *out)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(d.FullSummary())
	for _, f := range files {
		fmt.Println("  ", f)
	}
}
