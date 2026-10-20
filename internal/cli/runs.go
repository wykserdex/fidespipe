package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/wykserdex/fidespipe/internal/journal"
)

func RunRunsList(args []string) {
	runsDir := journal.DefaultRunsDir()
	storeType := "fs"
	dbPath := ""
	for _, a := range args {
		if len(a) > 11 && a[:11] == "--runs-dir=" {
			runsDir = a[11:]
		} else if len(a) > 8 && a[:8] == "--store=" {
			storeType = a[8:]
		} else if len(a) > 9 && a[:9] == "--db-path=" {
			dbPath = a[9:]
		} else if !isFlag(a) {
			runsDir = a
		}
	}
	var store journal.RunStore
	if storeType == "json" {
		store = journal.NewJsonStore(runsDir, dbPath)
	} else {
		store = journal.NewFilesystemStore(runsDir)
	}
	ids, err := store.ListRuns()
	if err != nil {
		fmt.Println("нет прогонов:", err)
		return
	}
	fmt.Printf("Прогоны в %s (store=%s):\n", runsDir, storeType)
	for _, id := range ids {
		dir, err := journal.SafeRunDir(runsDir, id)
		if err != nil {
			continue
		}
		rd := journal.NewReader(dir)
		events, _ := rd.Events()
		pipelineName := ""
		if len(events) > 0 {
			if pn, ok := events[0]["pipeline"].(string); ok {
				pipelineName = pn
			}
		}
		arts, _ := store.ListArtifacts(id)
		fmt.Printf("  %-40s  pipeline=%-20s  events=%d  artifacts=%d\n", id, pipelineName, len(events), len(arts))
	}
}

func RunRunsShow(args []string) {
	if len(args) < 1 {
		fmt.Println("нужен id прогона: orchestrator runs show <run_id>")
		os.Exit(2)
	}
	id := args[0]
	runsDir := journal.DefaultRunsDir()
	storeType := "fs"
	dbPath := ""
	for _, a := range args[1:] {
		if len(a) > 11 && a[:11] == "--runs-dir=" {
			runsDir = a[11:]
		} else if len(a) > 8 && a[:8] == "--store=" {
			storeType = a[8:]
		} else if len(a) > 9 && a[:9] == "--db-path=" {
			dbPath = a[9:]
		} else if !isFlag(a) {
			runsDir = a
		}
	}
	var store journal.RunStore
	if storeType == "json" {
		store = journal.NewJsonStore(runsDir, dbPath)
	} else {
		store = journal.NewFilesystemStore(runsDir)
	}
	dir, err := journal.SafeRunDir(runsDir, id)
	if err != nil {
		fmt.Println("ошибка чтения прогона:", err)
		os.Exit(1)
	}
	rd := journal.NewReader(dir)
	events, err := rd.Events()
	if err != nil {
		fmt.Println("ошибка чтения журнала:", err)
		os.Exit(1)
	}
	snap, _ := rd.ContextSnapshot()
	fmt.Printf("Run %s (%s) store=%s:\n", id, dir, storeType)
	fmt.Printf("  events: %d\n", len(events))
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		fmt.Println("  ", string(b))
	}
	if snap != nil {
		fmt.Println("\nContext snapshot:")
		b, _ := json.MarshalIndent(snap, "", "  ")
		fmt.Println(string(b))
	}
	arts, _ := store.ListArtifacts(id)
	if len(arts) > 0 {
		fmt.Printf("\nArtifacts (%d):\n", len(arts))
		for _, a := range arts {
			fmt.Println("  -", a)
		}
	}
}

func RunRunsResume(args []string) {
	if len(args) < 1 {
		fmt.Println("нужен id прогона: orchestrator runs resume <run_id> <pipeline.yaml>")
		os.Exit(2)
	}
	runID := args[0]
	if err := journal.ValidateRunID(runID); err != nil {
		fmt.Println("ошибка resume:", err)
		os.Exit(1)
	}
	pipelineFile := ""
	yes := false
	noAuto := false
	denyUntrusted := false
	allowUntrusted := false
	trustCfg := ""
	runsDir := ""
	store := ""
	dbPath := ""
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			continue
		case a == "--yes":
			yes = true
		case a == "--no-auto-approve":
			noAuto = true
		case a == "--deny-untrusted-plugins":
			denyUntrusted = true
		case a == "--allow-untrusted-plugins":
			allowUntrusted = true
		case strings.HasPrefix(a, "--trust-config="):
			trustCfg = strings.TrimPrefix(a, "--trust-config=")
		case strings.HasPrefix(a, "--runs-dir="):
			runsDir = strings.TrimPrefix(a, "--runs-dir=")
		case a == "--runs-dir":
			if i+1 >= len(args) {
				fmt.Println("флагу --runs-dir нужно значение")
				os.Exit(2)
			}
			i++
			runsDir = args[i]
		case strings.HasPrefix(a, "--store="):
			store = strings.TrimPrefix(a, "--store=")
		case a == "--store":
			if i+1 >= len(args) {
				fmt.Println("флагу --store нужно значение")
				os.Exit(2)
			}
			i++
			store = args[i]
		case strings.HasPrefix(a, "--db-path="):
			dbPath = strings.TrimPrefix(a, "--db-path=")
		case a == "--db-path":
			if i+1 >= len(args) {
				fmt.Println("флагу --db-path нужно значение")
				os.Exit(2)
			}
			i++
			dbPath = args[i]
		case strings.HasPrefix(a, "-"):
			fmt.Printf("неизвестный флаг runs resume %q\n", a)
			os.Exit(2)
		case pipelineFile == "":
			pipelineFile = a
		default:
			fmt.Printf("лишний аргумент runs resume %q\n", a)
			os.Exit(2)
		}
	}
	if pipelineFile == "" {
		fmt.Println("нужен файл пайплайна для resume: orchestrator runs resume <run_id> <pipeline.yaml> [--yes]")
		os.Exit(2)
	}
	newArgs := []string{pipelineFile}
	if yes {
		newArgs = append(newArgs, "--yes")
	}
	if noAuto {
		newArgs = append(newArgs, "--no-auto-approve")
	}
	if denyUntrusted {
		newArgs = append(newArgs, "--deny-untrusted-plugins")
	}
	if allowUntrusted {
		newArgs = append(newArgs, "--allow-untrusted-plugins")
	}
	// Конфиг доверия пробрасывается в ран: без него resume поднялся бы с пустым
	// allow-list и упал бы на первом же плагине, который resume вправе запустить.
	if trustCfg != "" {
		newArgs = append(newArgs, "--trust-config="+trustCfg)
	}
	if runsDir != "" {
		newArgs = append(newArgs, "--runs-dir="+runsDir)
	}
	if store != "" {
		newArgs = append(newArgs, "--store="+store)
	}
	if dbPath != "" {
		newArgs = append(newArgs, "--db-path="+dbPath)
	}
	newArgs = append(newArgs, "--resume="+runID)
	RunPipelineRun(newArgs)
}

func isFlag(s string) bool {
	return len(s) > 0 && s[0] == '-'
}
