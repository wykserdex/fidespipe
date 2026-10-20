package execution

import (
	"testing"

	"github.com/wykserdex/fidespipe/internal/journal"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

// builtinEngine — настоящий движок, а не permissiveEngine. Тест ровно про то,
// что манифест встроенного модуля выдаёт реестр: подставной движок отдаёт
// пустой манифест без портов, buildInput нечего заполнять, и шаг падает с
// empty_text на любом тексте — то есть тест проверял бы не модуль.
func builtinEngine() Engine { return plugin.NewEngine() }

// Сквозной путь core/text_stats: шаг отрабатывает, выход доступен как steps.*,
// и его видно в форме гейта. Тот же путь, что у внешнего плагина, только без
// subprocess.
func TestBuiltinTextStatsRunsEndToEnd(t *testing.T) {
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:    "builtin_e2e",
			Network: "deny",
			Input:   map[string]interface{}{"text": "раз два три"},
			Steps: []pipeline.Step{
				{ID: "stats", Plugin: "core/text_stats", OnError: "stop"},
				{
					ID: "review", Plugin: "core/human_gate",
					Form: []pipeline.FormField{
						{Field: "steps.stats.words", Editable: false, Type: "number"},
						{Field: "steps.stats.longest_word", Editable: false, Type: "string"},
					},
					Actions: []string{"accept", "reject"}, OnReject: "stop",
				},
			},
		},
	}
	stats, err := Run(pf, builtinEngine(), RunOptions{Yes: true, Quiet: true, RunsDir: t.TempDir()})
	if err != nil {
		t.Fatalf("встроенный модуль должен отработать: %+v", err)
	}
	if stats.Aborted != 0 {
		for _, e := range runEvents(t, stats.RunDir) {
			t.Logf("событие: %v", e)
		}
		t.Fatalf("aborted = %d, ожидался 0", stats.Aborted)
	}
	// Выход обязан попасть в namespace: без ctx.SetStep гейт увидел бы пустую
	// форму. Проверяем через снапшот, а не только по «ран не упал».
	snap, err := journal.NewReader(stats.RunDir).ContextSnapshot()
	if err != nil {
		t.Fatalf("снапшот контекста не читается: %v", err)
	}
	steps, _ := snap["steps"].(map[string]interface{})
	out, ok := steps["stats"].(map[string]interface{})
	if !ok {
		t.Fatalf("выхода шага stats нет в снапшоте: %v", snap["steps"])
	}
	// Числа из context.json приходят как float64 — JSON не различает целые.
	if got, _ := out["words"].(float64); got != 3 {
		t.Errorf("words = %v (%T), ожидалось 3", out["words"], out["words"])
	}
	if got, _ := out["longest_word"].(string); got != "раз" {
		t.Errorf("longest_word = %v, ожидалось %q — все слова длиной 3, берётся первое", out["longest_word"], "раз")
	}
}

// Пустой текст обязан дать доменную ошибку, а не тихо вернуть нули: иначе
// пайплайн с условием «больше 10 слов» тихо пошёл бы по другой ветке.
//
// Форма отказа та же, что у внешнего плагина: abort элемента, а не падение
// ран-функции. Проверяем aborted, потому что это и есть наблюдаемое поведение
// для on_error=stop без foreach.
func TestBuiltinTextStatsEmptyTextAbortsItem(t *testing.T) {
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:    "builtin_empty",
			Network: "deny",
			Input:   map[string]interface{}{"text": "   "},
			Steps: []pipeline.Step{
				{ID: "stats", Plugin: "core/text_stats", OnError: "stop"},
			},
		},
	}
	stats, err := Run(pf, builtinEngine(), RunOptions{Yes: true, Quiet: true, RunsDir: t.TempDir()})
	if err != nil {
		t.Fatalf("on_error=stop без foreach не роняет ран-функцию (как у плагина): %+v", err)
	}
	if stats.Aborted != 1 {
		t.Fatalf("aborted = %d, ожидался 1", stats.Aborted)
	}
	var sawCode string
	for _, e := range runEvents(t, stats.RunDir) {
		if e["type"] == "step_failed" {
			sawCode, _ = e["code"].(string)
		}
	}
	if sawCode != "empty_text" {
		t.Errorf("код в step_failed = %q, ожидался empty_text", sawCode)
	}
}

// on_error: skip у встроенного модуля обязан вести себя как у плагина: ран
// продолжается, значение шага не остаётся в namespace.
func TestBuiltinTextStatsOnErrorSkipContinues(t *testing.T) {
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:    "builtin_skip",
			Network: "deny",
			Input:   map[string]interface{}{"text": ""},
			Steps: []pipeline.Step{
				{ID: "stats", Plugin: "core/text_stats", OnError: "skip"},
				{ID: "review", Plugin: "core/human_gate", Actions: []string{"accept"}, OnReject: "stop"},
			},
		},
	}
	stats, err := Run(pf, builtinEngine(), RunOptions{Yes: true, Quiet: true, RunsDir: t.TempDir()})
	if err != nil {
		t.Fatalf("on_error=skip обязан продолжать ран: %+v", err)
	}
	if stats.Aborted != 0 {
		t.Fatalf("aborted = %d, ожидался 0 (skip не отменяет элемент)", stats.Aborted)
	}
	// Пропущенный шаг не должен оставить значение: иначе в foreach-агрегацию
	// утекла бы итерация предыдущего элемента.
	snap, serr := journal.NewReader(stats.RunDir).ContextSnapshot()
	if serr != nil {
		t.Fatalf("снапшот контекста не читается: %v", serr)
	}
	steps, _ := snap["steps"].(map[string]interface{})
	if _, leaked := steps["stats"]; leaked {
		t.Errorf("значение пропущенного шага осталось в namespace: %v", steps["stats"])
	}
}

// Гейт остаётся гейтом: ветка «builtin значит гейт» была безусловной, и
// core/text_stats молча ушёл бы в gate.Service.
func TestHumanGateStillIsGate(t *testing.T) {
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "gate_only",
			Input: map[string]interface{}{"text": "x"},
			Steps: []pipeline.Step{
				{ID: "review", Plugin: "core/human_gate", Actions: []string{"accept"}, OnReject: "stop"},
			},
		},
	}
	stats, err := Run(pf, builtinEngine(), RunOptions{Yes: true, Quiet: true, RunsDir: t.TempDir()})
	if err != nil {
		t.Fatalf("гейт должен отработать: %+v", err)
	}
	if stats.Aborted != 0 {
		t.Fatalf("aborted = %d, ожидался 0", stats.Aborted)
	}
}

// Закрытость namespace: core/*, которого нет в allowlist, обязан отвергаться,
// иначе «builtin» перестаёт быть закрытым множеством.
func TestUnknownBuiltinStillRefused(t *testing.T) {
	pf := &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:  "unknown_builtin",
			Input: map[string]interface{}{"text": "x"},
			Steps: []pipeline.Step{
				{ID: "s", Plugin: "core/text_statz", OnError: "stop"},
			},
		},
	}
	if _, err := Run(pf, builtinEngine(), RunOptions{Yes: true, Quiet: true, RunsDir: t.TempDir()}); err == nil {
		t.Fatal("неизвестный core/* обязан быть отвергнут")
	}
}
