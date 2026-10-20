package execution

import (
	"errors"
	"regexp"
	"strings"

	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/plugin"
	"github.com/wykserdex/fidespipe/internal/runctx"
)

// core/text_stats — встроенный модуль: метрики текста.
//
// Зачем он встроен, а не ещё один community-плагин: до него у Fidespipe не было ни
// одной встроенной полезной функции — единственным builtin был human_gate. Любой
// осмысленный пайплайн требовал внешнего плагина, а значит клона репозитория и
// Python в PATH. Встроенный модуль исполняется в процессе ядра, поэтому первая
// полезная цепочка работает на одном бинарнике.
//
// Контракт и семантика совпадают с community-плагином text_analyzer, чтобы его
// можно было заменить без правки пайплайна и без расхождения чисел.

// errEmptyText — доменная ошибка шага, а не ранняя: on_error решает её судьбу
// так же, как решил бы для внешнего плагина.
var errEmptyText = errors.New("поле text пустое — нечего анализировать")

// wordPattern — \w из Python с re.UNICODE: буквы, цифры, подчёркивание. В Go
// \w по умолчанию только ASCII, из-за чего «Оркестратор» распался бы на
// отдельные символы, а счётчик слов врал бы на любом не-латинском тексте.
var wordPattern = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// textStats — чистое вычисление, без платформенной обвязки: журналом,
// on_error и контрактом выхода занимается runBuiltinDataStep, ровно так же,
// как для внешнего плагина.
func textStats(text string) (map[string]interface{}, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, errEmptyText
	}
	words := wordPattern.FindAllString(trimmed, -1)
	unique := make(map[string]struct{}, len(words))
	longest := ""
	for _, w := range words {
		unique[strings.ToLower(w)] = struct{}{}
		// len в Python считает кодовые точки, поэтому rune, а не байты.
		// Строгое «>» сохраняет ПЕРВЫЙ из равных — как max(words, key=len).
		if longest == "" || len([]rune(w)) > len([]rune(longest)) {
			longest = w
		}
	}
	return map[string]interface{}{
		"lines":        strings.Count(trimmed, "\n") + 1,
		"words":        len(words),
		"unique_words": len(unique),
		"longest_word": longest,
	}, nil
}

// runBuiltinDataStep — шаг встроенного модуля, который не гейт.
//
// Вход собирается тем же buildInput, выход проходит тем же
// plugin.EnforceOutput, ошибки идут через тот же on_error, а события журнала
// называются так же. Иначе встроенный модуль вёл бы себя иначе, чем плагин,
// ровно в тех местах, где расхождение потом всего дороже всего ищется.
func runBuiltinDataStep(eng Engine, st *pipeline.Step, ctx *runctx.Ctx, j *journal.Journal, opts RunOptions) (string, error) {
	m, err := eng.LoadManifest(st.Plugin)
	if err != nil {
		return "", runErr("E_PLUGIN_LOAD", "шаг %s: %v", st.ID, err)
	}
	in, err := buildInput(m, st, ctx)
	if err != nil {
		return builtinStepFailed(st, ctx, j, opts, "contract_input", err)
	}
	text := ""
	for _, v := range in {
		if s, ok := v.(string); ok {
			text = s
			break
		}
	}
	out, err := textStats(text)
	if err != nil {
		return builtinStepFailed(st, ctx, j, opts, "empty_text", err)
	}
	checked, dropped, err := plugin.EnforceOutput(m, out)
	if len(dropped) > 0 {
		j.Event("contract_warning", map[string]interface{}{"step": st.ID, "dropped_fields": dropped})
	}
	if err != nil {
		// PROTOCOL §5: нарушение контракта выхода — платформенная ошибка.
		return "", &RunError{Code: "contract_output", Err: err}
	}
	ctx.SetStep(st.ID, checked)
	return "ok", nil
}

// builtinStepFailed — единая судьба ошибки для встроенного шага, дословно как у
// внешнего плагина: skip чистит namespace шага, иначе элемент останавливается.
func builtinStepFailed(st *pipeline.Step, ctx *runctx.Ctx, j *journal.Journal, opts RunOptions, code string, cause error) (string, error) {
	if st.OnError == "skip" {
		opts.logf("    ! пропущен по on_error=skip: %s", cause)
		j.Event("step_skipped", map[string]interface{}{"step": st.ID, "reason": "on_error", "code": code, "message": cause.Error()})
		if stepsMap, ok := ctx.Data["steps"].(map[string]interface{}); ok {
			delete(stepsMap, st.ID)
		}
		return "ok", nil
	}
	opts.logf("    × %s: %s — элемент остановлен", code, cause)
	j.Event("step_failed", map[string]interface{}{"step": st.ID, "code": code, "message": cause.Error()})
	return "abort_item", nil
}
