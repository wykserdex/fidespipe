package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/wykserdex/fidespipe/internal/core"
	"github.com/wykserdex/fidespipe/internal/execution"
	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// RunDemo — автономная демонстрация: одна цепочка, ноль prerequisites.
//
// Зачем: до 0.33a у Fidespipe не было ни одной встроенной полезной функции, поэтому
// первая осмысленная цепочка требовала клона репозитория и Python в PATH. Quick
// Start честно это отражал, но «скачай бинарник и посмотри» начиналось с
// `git clone`. Здесь же нужны только сам бинарник и его каталог для журнала.
//
// Обещание, которое команда выполняет: полный ран с гейтом человека офлайн, без
// git, без Python, без сети. Всё встроенное, поэтому песочница не нужна.
func RunDemo(args []string) {
	runsDir := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--runs-dir="):
			runsDir = strings.TrimPrefix(a, "--runs-dir=")
		case a == "--runs-dir" && i+1 < len(args):
			i++
			runsDir = args[i]
		default:
			fmt.Printf("неизвестный флаг demo %q\n", a)
			os.Exit(2)
		}
	}
	autoDir := runsDir == ""
	if autoDir {
		dir, err := os.MkdirTemp("", "fidespipe-demo-")
		if err != nil {
			fmt.Println("не удалось создать каталог для демо:", err)
			os.Exit(2)
		}
		runsDir = dir
	}
	eng := core.NewEngine()
	pf := demoPipeline()
	if errs, warns := core.Validate(pf, eng); len(errs) > 0 || len(warns) > 0 {
		for _, w := range warns {
			fmt.Println("  · предупреждение:", w)
		}
		for _, e := range errs {
			fmt.Println("  ✗", e)
		}
		os.Exit(1)
	}
	fmt.Println("Fidespipe · демо. Ни git, ни Python, ни сети — всё встроенное.")
	fmt.Println("Текст → метрики → человек подтверждает решение.")
	fmt.Println()

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		cancel()
		<-sig
		os.Exit(130)
	}()

	stats, err := core.Run(pf, eng, core.RunOptions{
		Yes:     true, // демо не должно упираться в TTY: гейт подтверждается сам
		RunsDir: runsDir,
		Ctx:     runCtx,
	})
	if err != nil {
		if errors.Is(err, execution.ErrCancelled) {
			fmt.Println("ран отменён; продолжить:", "fidespipe runs resume", runIDFromDir(stats.RunDir), runsDir)
			os.Exit(130)
		}
		fmt.Println("ран упал:", err)
		os.Exit(1)
	}
	if stats.Aborted > 0 {
		fmt.Println("ран завершился с отклонёнными шагами:", stats.Aborted)
		os.Exit(1)
	}
	fmt.Println()
	runID := runIDFromDir(stats.RunDir)
	fmt.Println("Что дальше — тоже без сети и без Python:")
	fmt.Printf("  fidespipe runs show %s %s   # журнал каждого шага\n", runID, runsDir)
	fmt.Println("  fidespipe plugin create ./my_plugin       # свой плагин по контракту")
	fmt.Println("  fidespipe pipeline validate <file.yaml>   # проверка до запуска")
	if autoDir {
		fmt.Println()
		fmt.Println("  Каталог с журналом временный. Своё место: fidespipe demo --runs-dir ./var/runs")
	}
}

// demoPipeline — цепочка, которая работает на одном бинарнике: встроенный
// text_stats, встроенный гейт, никаких внешних плагинов и никакой сети.
func demoPipeline() *pipeline.PipelineFile {
	return &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name: "demo",
			// Явно deny: unset означает столько же, но в демо видно намерение,
			// и пример совпадает с тем, что напечатан в README.
			Network: "deny",
			Input: map[string]interface{}{
				"text": "Оркестратор цепочек\n" +
					"Каждый шаг пишет свой журнал\n" +
					"Решение остаётся за человеком",
			},
			Steps: []pipeline.Step{
				{
					ID: "stats", Plugin: "core/text_stats", OnError: "stop",
				},
				{
					ID: "review", Plugin: "core/human_gate",
					Form: []pipeline.FormField{
						{Field: "steps.stats.lines", Editable: false, Type: "number"},
						{Field: "steps.stats.words", Editable: false, Type: "number"},
						{Field: "steps.stats.unique_words", Editable: false, Type: "number"},
						{Field: "steps.stats.longest_word", Editable: false, Type: "string"},
					},
					Actions:  []string{"accept", "reject"},
					OnReject: "stop",
				},
			},
		},
	}
}
