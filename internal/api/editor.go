package api

// v0.25: редактор пайплайнов — парсинг/сериализация через Go (JS тонкий,
// корректность YAML живёт в ядре, а не в браузере).
//
// Модель редактора (EditorDoc) покрывает схему pipeline: id/plugin/pos/bind/
// on_error/timeout, управляющий поток, input-значения и input-схемы.
// Позиции узлов хранятся в YAML как `pos: [x, y]` и читаются ядром.
// v0.27: when — под управлением редактора (path/op/value, 10 операторов ядра).
// v0.28: foreach/parallel_group/after_foreach на шаге + foreach/foreach_item/
// item_type/item_format на пайплайне (управляющий поток целиком).
// v0.29: retry (on_error: retry + retry{attempts, delay, backoff}).
// v0.5: secrets (pipeline.secrets — env-ключи плагинов) под управлением
// редактора; кросс-чек «плагин ↔ пайплайн» — валидатор ядра (warnings).
// v0.6: network (pipeline.network — политика allow/deny) под управлением
// редактора; кросс-чек «плагин заявил сеть + deny» — ошибка валидатора.
// v0.7: type-объявления input (type/required/default/format/description) под
// управлением редактора; неподдерживаемые map-значения остаются обычным JSON.

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"

	"gopkg.in/yaml.v3"
)

type editorInput struct {
	Name        string      `json:"name"`
	Default     interface{} `json:"default"`
	Typed       bool        `json:"typed,omitempty"`
	Type        string      `json:"type,omitempty"`
	Required    *bool       `json:"required,omitempty"`
	Format      string      `json:"format,omitempty"`
	Description string      `json:"description,omitempty"`
	HasDefault  bool        `json:"has_default"`
}

type editorFormField struct {
	Field    string `json:"field" yaml:"field"`
	Editable bool   `json:"editable" yaml:"editable"`
	Type     string `json:"type,omitempty" yaml:"type,omitempty"`
	Format   string `json:"format,omitempty" yaml:"format,omitempty"`
}

// editorWhen — v0.27: условие шага (ядро: internal/pipeline/when.go,
// операторы WhenOps). Value — как ввёл пользователь (строка из UI),
// на выходе коэрсируется для числовых операторов (см. serialize).
type editorWhen struct {
	Path  string      `json:"path"`
	Op    string      `json:"op"`
	Value interface{} `json:"value,omitempty"`
}

type editorStep struct {
	ID       string            `json:"id"`
	Plugin   string            `json:"plugin"`
	Pos      [2]int            `json:"pos"`
	OnError  string            `json:"on_error"`
	Timeout  string            `json:"timeout"`
	Bind     map[string]string `json:"bind"`
	Form     []editorFormField `json:"form"`
	Actions  []string          `json:"actions"`
	OnReject string            `json:"on_reject"`
	Approval string            `json:"approval"`
	When     *editorWhen       `json:"when,omitempty"`
	// v0.28: управляющий поток шага
	Foreach       string `json:"foreach"`
	ForeachItem   string `json:"foreach_item"`
	AfterForeach  bool   `json:"after_foreach"`
	ParallelGroup string `json:"parallel_group"`
	// Управляющий цикл шага.
	Loop          string `json:"loop"`
	LoopCondition string `json:"loop_condition"`
	MaxIterations int    `json:"max_iterations"`
	// v0.29: retry (вместе с on_error: retry)
	Retry *editorRetry `json:"retry,omitempty"`
}

// editorRetry — v0.29: политика повторов (ядро: PROTOCOL §5: retry повторяет
// таймауты и доменные ошибки с retryable: true; исчерпанный retry = stop).
type editorRetry struct {
	Attempts int    `json:"attempts"`
	Delay    string `json:"delay"`   // "5s", "100ms", … (time.ParseDuration)
	Backoff  string `json:"backoff"` // fixed | exponential
}

type editorDoc struct {
	Name          string        `json:"name"`
	FormatVersion string        `json:"format_version"` // v0.26a: сохраняется из исходника (пусто = новое → 0.2)
	Input         []editorInput `json:"input"`
	Steps         []editorStep  `json:"steps"`
	// v0.5: env-ключи, которые ядро передаст плагинам (кросс-чек с
	// permissions.secrets манифестов — валидатор, warnings)
	Secrets []string `json:"secrets"`
	// v0.6: сетевая политика — "" (allow, дефолт: плагин видит
	// FIDESPIPE_NETWORK=allow и сам декларирует сеть в манифесте) или "deny"
	// (шаг с заявленной сетью — ошибка валидатора и раннера)
	Network     string   `json:"network"`
	Gates       string   `json:"gates"`
	Unsupported []string `json:"unsupported"`
	// v0.28: управляющий поток пайплайна (батч по массиву)
	Foreach     string `json:"foreach"`
	ForeachItem string `json:"foreach_item"`
	ItemType    string `json:"item_type"`
	ItemFormat  string `json:"item_format"`
}

func supportedEditorInputType(typeName string) bool {
	switch typeName {
	case "string", "number", "boolean", "array", "object":
		return true
	default:
		return false
	}
}

func isInputTypeDeclaration(value interface{}) bool {
	descriptor, ok := value.(map[string]interface{})
	if !ok {
		return false
	}
	typeName, ok := descriptor["type"].(string)
	if !ok || !supportedEditorInputType(typeName) {
		return false
	}
	for key, value := range descriptor {
		switch key {
		case "type":
		case "required":
			if _, ok := value.(bool); !ok {
				return false
			}
		case "default":
		case "format", "description":
			if _, ok := value.(string); !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *Server) handleParsePipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST yaml", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pf, err := pipeline.LoadPipelineFileFromBytes(data)
	if err != nil {
		http.Error(w, "parse: "+err.Error(), 400)
		return
	}
	doc := editorDoc{
		Name:          pf.Pipeline.Name,
		FormatVersion: pf.FormatVersion,
		Input:         []editorInput{},
		Steps:         []editorStep{},
		Unsupported:   []string{},
		Foreach:       pf.Pipeline.Foreach,
		ForeachItem:   pf.Pipeline.ForeachItem,
		ItemType:      pf.Pipeline.ItemType,
		ItemFormat:    pf.Pipeline.ItemFormat,
		Gates:         pf.Pipeline.Gates,
	}
	names := make([]string, 0, len(pf.Pipeline.Input))
	for n := range pf.Pipeline.Input {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := pf.Pipeline.Input[n]
		if isInputTypeDeclaration(v) {
			descriptor := v.(map[string]interface{})
			in := editorInput{Name: n, Typed: true, Type: descriptor["type"].(string)}
			if value, ok := descriptor["default"]; ok {
				in.Default, in.HasDefault = value, true
			}
			if required, ok := descriptor["required"].(bool); ok {
				in.Required = &required
			}
			if format, ok := descriptor["format"].(string); ok {
				in.Format = format
			}
			if description, ok := descriptor["description"].(string); ok {
				in.Description = description
			}
			doc.Input = append(doc.Input, in)
		} else {
			doc.Input = append(doc.Input, editorInput{Name: n, Default: v, HasDefault: true})
		}
	}
	for _, st := range pf.Pipeline.Steps {
		es := editorStep{
			ID:      st.ID,
			Plugin:  st.Plugin,
			Pos:     st.Pos,
			OnError: st.OnError,
			Bind:    map[string]string{},
		}
		if es.OnError == "" {
			es.OnError = "stop"
		}
		if st.Timeout.Duration > 0 {
			es.Timeout = st.Timeout.Duration.String()
		}
		for k, v := range st.Bind {
			es.Bind[k] = v
		}
		for _, f := range st.Form {
			es.Form = append(es.Form, editorFormField{Field: f.Field, Editable: f.Editable, Type: f.Type, Format: f.Format})
		}
		es.Actions = st.Actions
		es.OnReject = st.OnReject
		es.Approval = st.Approval
		if st.When.IsSet() {
			es.When = &editorWhen{Path: st.When.Path, Op: st.When.Op, Value: st.When.Value}
		}
		es.Foreach = st.Foreach
		es.ForeachItem = st.ForeachItem
		es.AfterForeach = st.AfterForeach
		es.ParallelGroup = st.ParallelGroup
		es.Loop = st.Loop
		es.LoopCondition = st.LoopCondition
		es.MaxIterations = st.MaxIterations
		if st.Retry != nil {
			re := &editorRetry{Attempts: st.Retry.Attempts, Backoff: st.Retry.Backoff}
			if st.Retry.Delay.Duration > 0 {
				re.Delay = st.Retry.Delay.Duration.String()
			}
			es.Retry = re
		}
		doc.Steps = append(doc.Steps, es)
	}
	// v0.5: secrets под управлением редактора (was: unsupported)
	if pf.Pipeline.Secrets != nil {
		doc.Secrets = pf.Pipeline.Secrets
	} else {
		doc.Secrets = []string{}
	}
	// v0.6: network под управлением редактора (was: unsupported)
	doc.Network = pf.Pipeline.Network
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

// outStep — теневой вывод: pos в YAML, остальное — ровно по схеме.
type outStep struct {
	ID       string            `yaml:"id"`
	Plugin   string            `yaml:"plugin"`
	Pos      [2]int            `yaml:"pos"`
	Bind     map[string]string `yaml:"bind,omitempty"`
	OnError  string            `yaml:"on_error,omitempty"`
	Timeout  string            `yaml:"timeout,omitempty"`
	Form     []editorFormField `yaml:"form,omitempty"`
	Actions  []string          `yaml:"actions,omitempty"`
	OnReject string            `yaml:"on_reject,omitempty"`
	Approval string            `yaml:"approval,omitempty"`
	When     *outWhen          `yaml:"when,omitempty"`
	// v0.28: управляющий поток шага
	Foreach       string    `yaml:"foreach,omitempty"`
	ForeachItem   string    `yaml:"foreach_item,omitempty"`
	AfterForeach  bool      `yaml:"after_foreach,omitempty"`
	ParallelGroup string    `yaml:"parallel_group,omitempty"`
	Loop          string    `yaml:"loop,omitempty"`
	LoopCondition string    `yaml:"loop_condition,omitempty"`
	MaxIterations int       `yaml:"max_iterations,omitempty"`
	Retry         *outRetry `yaml:"retry,omitempty"`
}

type outRetry struct {
	Attempts int    `yaml:"attempts"`
	Delay    string `yaml:"delay,omitempty"`
	Backoff  string `yaml:"backoff,omitempty"`
}

type outWhen struct {
	Path  string      `yaml:"path"`
	Op    string      `yaml:"op"`
	Value interface{} `yaml:"value,omitempty"`
}

type outFile struct {
	FormatVersion string `yaml:"format_version"`
	Pipeline      struct {
		Name        string         `yaml:"name"`
		Input       map[string]any `yaml:"input,omitempty"`
		Steps       []outStep      `yaml:"steps"`
		Secrets     []string       `yaml:"secrets,omitempty"` // v0.5
		Network     string         `yaml:"network,omitempty"` // v0.6
		Gates       string         `yaml:"gates,omitempty"`
		Foreach     string         `yaml:"foreach,omitempty"`
		ForeachItem string         `yaml:"foreach_item,omitempty"`
		ItemType    string         `yaml:"item_type,omitempty"`
		ItemFormat  string         `yaml:"item_format,omitempty"`
	} `yaml:"pipeline"`
}

func (s *Server) handleSerializePipeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST editor-doc json", http.StatusMethodNotAllowed)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var doc editorDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		http.Error(w, "json: "+err.Error(), 400)
		return
	}
	if len(doc.Unsupported) > 0 {
		http.Error(w, "пайплайн содержит поля, которые редактор не управляет: "+strings.Join(doc.Unsupported, ", ")+
			" — редактируй в YAML (вкладка Пайплайны), не из редактора", http.StatusConflict)
		return
	}
	// документ → схема ядра
	// v0.26a: версия формата — из исходного файла; новое doc (без версии) → текущая 0.2
	fv := doc.FormatVersion
	if fv == "" {
		fv = "0.2"
	}
	pf := pipeline.PipelineFile{
		FormatVersion: fv,
		Pipeline: pipeline.Pipeline{
			Name: doc.Name, Input: map[string]interface{}{}, Steps: []pipeline.Step{},
			Foreach: doc.Foreach, ForeachItem: doc.ForeachItem,
			ItemType: doc.ItemType, ItemFormat: doc.ItemFormat, Gates: doc.Gates,
		},
	}
	for _, in := range doc.Input {
		if in.Name == "" {
			continue
		}
		if !in.Typed {
			pf.Pipeline.Input[in.Name] = in.Default
			continue
		}
		if !supportedEditorInputType(in.Type) {
			http.Error(w, "input "+in.Name+": type должен быть string, number, boolean, array или object", http.StatusBadRequest)
			return
		}
		descriptor := map[string]interface{}{"type": in.Type}
		if in.Required != nil {
			descriptor["required"] = *in.Required
		}
		if in.HasDefault {
			descriptor["default"] = in.Default
		}
		if in.Format != "" {
			descriptor["format"] = in.Format
		}
		if in.Description != "" {
			descriptor["description"] = in.Description
		}
		pf.Pipeline.Input[in.Name] = descriptor
	}
	// v0.5: secrets из doc (пустые строки UI не валиден — отбрасываем)
	for _, k := range doc.Secrets {
		if key := strings.TrimSpace(k); key != "" {
			pf.Pipeline.Secrets = append(pf.Pipeline.Secrets, key)
		}
	}
	// v0.6: network — только две политики. Явный allow обязан пережить
	// round-trip: раньше редактор писал в YAML только deny, а allow
	// отбрасывал, считая его дефолтом. После перехода «пустое поле = deny»
	// это молча превращало разрешение в запрет — validate проходил, ран
	// отказывал. Пустое поле остаётся пустым (ничего не выдумываем), но
	// заданное значение сохраняется как есть.
	if net := strings.TrimSpace(doc.Network); net == pipeline.NetworkDeny || net == pipeline.NetworkAllow {
		pf.Pipeline.Network = net
	}
	for _, st := range doc.Steps {
		step := pipeline.Step{
			ID:      st.ID,
			Plugin:  st.Plugin,
			OnError: st.OnError,
			Bind:    map[string]string{},
		}
		if step.OnError == "" {
			step.OnError = "stop"
		}
		for k, v := range st.Bind {
			if k != "" && v != "" {
				step.Bind[k] = v
			}
		}
		if st.Timeout != "" {
			d, err := time.ParseDuration(st.Timeout)
			if err != nil {
				http.Error(w, "timeout "+st.ID+": "+err.Error(), 400)
				return
			}
			step.Timeout = pipeline.Duration{Duration: d}
		}
		for _, f := range st.Form {
			step.Form = append(step.Form, pipeline.FormField{Field: f.Field, Editable: f.Editable, Type: f.Type, Format: f.Format})
		}
		step.Actions = st.Actions
		step.OnReject = st.OnReject
		step.Approval = st.Approval
		if st.When != nil {
			w := pipeline.When{Path: st.When.Path, Op: st.When.Op, Value: st.When.Value}
			if w.Op == "" {
				w.Op = "truthy"
			}
			// пустая строка из UI = «значения нет» — не выводим в YAML
			if t, ok := w.Value.(string); ok && t == "" {
				w.Value = nil
			}
			// UI шлёт value строкой; числовые операторы ядра требуют число
			if w.Op == "gt" || w.Op == "gte" || w.Op == "lt" || w.Op == "lte" {
				if t, ok := w.Value.(string); ok {
					if f, err := strconv.ParseFloat(t, 64); err == nil {
						w.Value = f
					}
				}
			}
			step.When = w
		}
		step.Foreach = st.Foreach
		step.ForeachItem = st.ForeachItem
		step.AfterForeach = st.AfterForeach
		step.ParallelGroup = st.ParallelGroup
		step.Loop = st.Loop
		step.LoopCondition = st.LoopCondition
		step.MaxIterations = st.MaxIterations
		if st.Retry != nil {
			r := &pipeline.Retry{Attempts: st.Retry.Attempts, Backoff: st.Retry.Backoff}
			if st.Retry.Delay != "" {
				d, err := time.ParseDuration(st.Retry.Delay)
				if err != nil {
					http.Error(w, "retry "+st.ID+": delay: "+err.Error(), 400)
					return
				}
				r.Delay = pipeline.Duration{Duration: d}
			}
			step.Retry = r
		}
		pf.Pipeline.Steps = append(pf.Pipeline.Steps, step)
	}
	// схема → YAML (через теневой вывод, чтобы pos прописался)
	out := outFile{FormatVersion: pf.FormatVersion}
	out.Pipeline.Name = pf.Pipeline.Name
	out.Pipeline.Foreach = pf.Pipeline.Foreach
	out.Pipeline.ForeachItem = pf.Pipeline.ForeachItem
	out.Pipeline.ItemType = pf.Pipeline.ItemType
	out.Pipeline.ItemFormat = pf.Pipeline.ItemFormat
	out.Pipeline.Gates = pf.Pipeline.Gates
	out.Pipeline.Input = map[string]any{}
	for k, v := range pf.Pipeline.Input {
		out.Pipeline.Input[k] = v
	}
	if len(pf.Pipeline.Secrets) > 0 {
		out.Pipeline.Secrets = pf.Pipeline.Secrets
	}
	// omitempty: поле не задано — его нет. Явный allow обязан быть в YAML,
	// иначе готовый пайплайн после round-trip стал бы deny.
	out.Pipeline.Network = pf.Pipeline.Network
	for _, st := range pf.Pipeline.Steps {
		os := outStep{ID: st.ID, Plugin: st.Plugin, Pos: docStepPos(doc, st.ID), OnError: st.OnError}
		if st.Timeout.Duration > 0 {
			os.Timeout = st.Timeout.Duration.String()
		}
		for k, v := range st.Bind {
			if os.Bind == nil {
				os.Bind = map[string]string{}
			}
			os.Bind[k] = v
		}
		for _, f := range st.Form {
			os.Form = append(os.Form, editorFormField{Field: f.Field, Editable: f.Editable, Type: f.Type, Format: f.Format})
		}
		os.Actions = st.Actions
		os.OnReject = st.OnReject
		os.Approval = st.Approval
		if st.When.IsSet() {
			os.When = &outWhen{Path: st.When.Path, Op: st.When.Op, Value: st.When.Value}
		}
		os.Foreach = st.Foreach
		os.ForeachItem = st.ForeachItem
		os.AfterForeach = st.AfterForeach
		os.ParallelGroup = st.ParallelGroup
		os.Loop = st.Loop
		os.LoopCondition = st.LoopCondition
		os.MaxIterations = st.MaxIterations
		if st.Retry != nil {
			os.Retry = &outRetry{Attempts: st.Retry.Attempts, Delay: st.Retry.Delay.Duration.String(), Backoff: st.Retry.Backoff}
		}
		out.Pipeline.Steps = append(out.Pipeline.Steps, os)
	}
	raw, err := yaml.Marshal(&out)
	if err != nil {
		http.Error(w, "yaml: "+err.Error(), 500)
		return
	}
	text := "# создан в редакторе Fidespipe (v0.26); pos: — позиции узлов (ядро игнорирует)\n" + string(raw)
	// честность: сгенерированный YAML обязан читаться ядром и проходить валидацию
	check, err := pipeline.LoadPipelineFileFromBytes([]byte(text))
	if err != nil {
		http.Error(w, "генерированный YAML не читается: "+err.Error(), 500)
		return
	}
	issues := pipeline.ValidateIssues(check, s.Engine)
	errs, warns := pipeline.SplitIssues(issues)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"yaml": text, "errors": errs, "warnings": warns, "issues": issues, "ok": len(errs) == 0,
	})
}

func docStepPos(doc editorDoc, id string) [2]int {
	for _, st := range doc.Steps {
		if st.ID == id {
			return st.Pos
		}
	}
	return [2]int{}
}
