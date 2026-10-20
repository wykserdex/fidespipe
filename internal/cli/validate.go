package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/wykserdex/fidespipe/internal/core"
	"github.com/wykserdex/fidespipe/internal/pipeline"
)

func RunPipelineValidate(args []string) {
	file, flags := splitArgs(args)
	if file == "" {
		fmt.Println("нужен файл пайплайна: fidespipe pipeline validate <file.yaml> [--json]")
		os.Exit(2)
	}
	asJSON := flags["--json"]
	// lint определяет вызывающая подкоманда (handlePipeline), а не os.Args[2]
	isLint := flags["--lint"]
	raw, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("ошибка чтения:", err)
		os.Exit(2)
	}
	var pf core.PipelineFile
	loaded, err := pipeline.LoadPipelineFileFromBytes(raw)
	if err == nil {
		pf = *loaded
	}
	if err != nil {
		if asJSON {
			printJSON(map[string]interface{}{"ok": false, "error": "yaml: " + err.Error()})
		} else {
			fmt.Println("YAML ошибка:", err)
		}
		os.Exit(2)
	}
	var issues []pipeline.Issue
	if isLint {
		issues = pipeline.LintIssues(&pf, core.NewEngine(), "")
	} else {
		issues = pipeline.ValidateIssues(&pf, core.NewEngine())
	}
	errs, warns := pipeline.SplitIssues(issues)
	if asJSON {
		if issues == nil {
			issues = []pipeline.Issue{}
		}
		printJSON(map[string]interface{}{"ok": len(errs) == 0, "issues": issues})
		if len(errs) > 0 {
			os.Exit(1)
		}
		return
	}
	for _, w := range warns {
		fmt.Println("  · предупреждение:", w)
	}
	for _, e := range errs {
		fmt.Println("  ✗", e)
	}
	if len(errs) > 0 {
		os.Exit(1)
	}
	if isLint {
		fmt.Println("OK: lint пройден (включая file_ref)")
	} else {
		fmt.Println("OK: цепочка совместима")
	}
}

// splitArgs — первый позиционный аргумент + набор флагов (--x), в любом порядке.
func splitArgs(args []string) (string, map[string]bool) {
	flags := map[string]bool{}
	file := ""
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			flags[a] = true
		} else if file == "" {
			file = a
		}
	}
	return file, flags
}

func printJSON(v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func RunPipelinePlan(args []string) {
	file, flags := splitArgs(args)
	if file == "" {
		fmt.Println("нужен файл пайплайна: fidespipe pipeline plan <file.yaml> [--json]")
		os.Exit(2)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("ошибка чтения:", err)
		os.Exit(2)
	}
	pf, err := pipeline.LoadPipelineFileFromBytes(raw)
	if err != nil {
		fmt.Println("YAML ошибка:", err)
		os.Exit(2)
	}
	plan, err := pipeline.PlanPipeline(pf, core.NewEngine())
	if err != nil {
		fmt.Println("ошибка плана:", err)
		os.Exit(2)
	}
	if flags["--json"] {
		issues := pipeline.ValidateIssues(pf, core.NewEngine())
		errs, _ := pipeline.SplitIssues(issues)
		printJSON(map[string]interface{}{"ok": len(errs) == 0, "issues": issues, "pipeline": pf.Pipeline.Name, "dag": plan.DAG})
		if len(errs) > 0 {
			os.Exit(1)
		}
		return
	}
	fmt.Printf("Pipeline: %s\n", pf.Pipeline.Name)
	fmt.Printf("Input: %v\n", pf.Pipeline.Input)
	if pf.Pipeline.Foreach != "" {
		fmt.Printf("Foreach: %s (item=%s, type=%s, format=%s)\n", pf.Pipeline.Foreach, pf.Pipeline.ForeachItem, pf.Pipeline.ItemType, pf.Pipeline.ItemFormat)
	}
	fmt.Println("Steps (DAG):")
	for i, st := range pf.Pipeline.Steps {
		phase := "foreach"
		for _, n := range plan.DAG.Nodes {
			if n.ID == st.ID {
				phase = n.Phase
				break
			}
		}
		fmt.Printf("  %d. %s → %s (on_error=%s, phase=%s, bind=%v)\n", i+1, st.ID, st.Plugin, st.OnError, phase, st.Bind)
		if len(st.Form) > 0 {
			fmt.Printf("     form: %v\n", st.Form)
		}
	}
	fmt.Println("Edges:")
	for _, e := range plan.DAG.Edges {
		fmt.Printf("  %s → %s via %s\n", e.From, e.To, e.Via)
	}
	for _, w := range plan.Warnings {
		fmt.Println("  · предупреждение:", w)
	}
	if len(plan.Errors) > 0 {
		fmt.Println("Ошибки валидации:")
		for _, e := range plan.Errors {
			fmt.Println("  ✗", e)
		}
		os.Exit(1)
	}
	fmt.Println("Plan OK — зависимостей и циклов нет (проверка циклов: v0.13)")
}
