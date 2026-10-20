package execution

import (
	"github.com/wykserdex/fidespipe/internal/journal"
)

// resumeStepCursor — шаги, завершившиеся успешно, по журналу.
//
// Зачем это отдельное от resumeCursor: у курсора resume две разные единицы
// отсчёта, и для разных форм цепочки нужны разные.
//
//	foreach — единица это ЭЛЕМЕНТ. Шаги внутри элемента выполняются заново
//	  (каждый элемент проходит всю цепочку), и курсор по элементу — правильный.
//	линейная цепочка — это тот же foreach, но с ОДНИМ элементом на всю цепочку
//	  (items = []interface{}{nil}). Поэтому курсор по элементу для неё
//	  бессмысленен: он всегда ноль, и resume начинался с нуля.
//
// Для линейной цепочки единица отсчёта — ШАГ, и он виден в журнале как
// step_end со статусом ok.
//
// Ограничение, и оно существенное: журнал говорит, что шаг ЗАВЕРШИЛСЯ, но не
// говорит, что его выход попал в снапшот контекста. Если процесс убили, а не
// упал, снапшот отстаёт, и пропуск шага скормил бы дальше неполные данные.
// Поэтому свежесть проверяется отдельно — по наличию выхода шага в
// восстановленном контексте (см. Run).
func resumeStepCursor(dir string) (map[string]bool, error) {
	events, err := journal.NewReader(dir).Events()
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, ev := range events {
		if typ, _ := ev["type"].(string); typ != "step_end" {
			continue
		}
		if status, _ := ev["status"].(string); status != "ok" {
			continue
		}
		if id, _ := ev["step"].(string); id != "" {
			done[id] = true
		}
	}
	return done, nil
}
