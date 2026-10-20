package cron

// Долговечная очередь слотов планировщика.
//
// Идея и атомарный снимок состояния взяты из форка ropemaxbroplayer/Nomos
// (MIT, Copyright (c) 2026 jfglag34-png), файл internal/cli/fleet_store_linux.go
// там. Здесь очередь переписана под задачи планировщика Fidespipe: без модели
// «свой job», без YAML и без привязки к Linux, и с ключом слота вместо
// произвольного.
//
// Зачем она нужна. Демон тикает раз в минуту и запускает то, что подошло по
// cron. Пока запущенный ран жив, следующий тик его не тронет — это ловит файл
// .running.<пайплайн>.lock. Но если демон перезапустился, пока ран ещё шёл,
// лок-файл остался от старого процесса, и следующий тик запустил бы ТОТ ЖЕ
// слот заново. Для пайплайна с побочным эффектом это двойное действие.
//
// Очередь решает это честно: слот получает claim до запуска и отпускается
// после. Слот, который уже отработал, второй раз не запускается никогда — даже
// если демон упал и поднялся.
//
// Отдельное решение, которое стоит перенести как есть: работа, ЗАСТРЯВШАЯ в
// статусе running, после перезапуска помечается interrupted, и по ней уходит
// оповещение. Повторять такую работу нельзя — неизвестно, что она уже
// надела. Это прямо противоположно «перезапустить и посмотреть».

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Статусы слота.
const (
	slotRunning     = "running"
	slotDone        = "done"
	slotFailed      = "failed"
	slotInterrupted = "interrupted"
)

// queueTask — слот в очереди.
type queueTask struct {
	ID      string    `json:"id"`
	Key     string    `json:"key"`
	Job     string    `json:"job"`
	Status  string    `json:"status"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Notice  string    `json:"notice,omitempty"`
}

// queueDisk — состояние очереди на диске.
type queueDisk struct {
	Version int                   `json:"version"`
	Tasks   map[string]*queueTask `json:"tasks"`
	// Claims — ключ слота → id задачи. Ключ держится и после завершения:
	// это и есть «этот слот уже отработал, второй раз не запускать».
	Claims map[string]string `json:"claims"`
}

// queue — долговечная очередь.
//
// Правило одно и незыблемое: после неуверенной записи состояние больше не
// доверяется. Молча продолжать работу с разъезжающимся состоянием хуже, чем
// остановиться, — поэтому ошибка записи «отравляет» очередь, и следующий
// вызов вернёт её вместо того, чтобы продолжить как ни в чём не бывало.
type queue struct {
	mu    sync.Mutex
	dir   string
	state queueDisk
	fault error
}

// openQueue — открыть очередь в каталоге, создав его при необходимости.
//
// Состояние читается и ПРОВЕРЯЕТСЯ: битое состояние — это ошибка, а не
// повод начать с нуля и молча отработать слоты повторно.
func openQueue(dir string) (*queue, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	q := &queue{
		dir:   dir,
		state: queueDisk{Version: 1, Tasks: map[string]*queueTask{}, Claims: map[string]string{}},
	}
	raw, err := os.ReadFile(filepath.Join(dir, "queue.json"))
	if err == nil {
		if err := json.Unmarshal(raw, &q.state); err != nil {
			return nil, fmt.Errorf("состояние очереди повреждено: %w", err)
		}
		if q.state.Version != 1 || q.state.Tasks == nil || q.state.Claims == nil {
			return nil, fmt.Errorf("состояние очереди неверной формы")
		}
		for id, t := range q.state.Tasks {
			if t == nil || t.ID != id || t.Key == "" {
				return nil, fmt.Errorf("неверная запись очереди: %s", id)
			}
			switch t.Status {
			case slotRunning, slotDone, slotFailed, slotInterrupted:
			default:
				return nil, fmt.Errorf("неверный статус слота %q", t.Status)
			}
		}
		for key, id := range q.state.Claims {
			if q.state.Tasks[id] == nil {
				return nil, fmt.Errorf("висящий claim: ключ %q, задача %q", key, id)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return q, nil
}

// commit — атомарный снимок состояния.
//
// temp → запись → fsync → close → rename → fsync каталога. Именно в таком
// порядке: rename атомарен, а fsync каталога после него делает переименование
// переживающим падение питания. Без fsync каталога после rename снимок может
// не появиться — и очередь решит, что слот не отработал, и запустит его
// заново.
func (q *queue) commit() error {
	if q.fault != nil {
		return q.fault
	}
	raw, err := json.Marshal(q.state)
	if err != nil {
		q.fault = err
		return err
	}
	f, err := os.CreateTemp(q.dir, ".queue-*")
	if err != nil {
		q.fault = err
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, filepath.Join(q.dir, "queue.json"))
	}
	if err == nil {
		err = syncDir(q.dir)
	}
	if err != nil {
		q.fault = err
	}
	return err
}

// Claim — занять слот.
//
// Возвращает false, если слот с таким ключом уже отрабатывал или работает
// сейчас. Это и есть идемпотентность: «этот слот уже запускался» — и второй
// раз он не запустится, что бы ни случилось с демоном.
func (q *queue) Claim(key, job string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fault != nil {
		return false, q.fault
	}
	if key == "" || job == "" {
		return false, fmt.Errorf("слот без ключа или без задачи")
	}
	if _, taken := q.state.Claims[key]; taken {
		return false, nil
	}
	now := time.Now().UTC()
	id := key
	t := &queueTask{ID: id, Key: key, Job: job, Status: slotRunning, Created: now, Updated: now}
	q.state.Tasks[id] = t
	q.state.Claims[key] = id
	if err := q.commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Release — отпустить слот с итогом.
//
// claim при этом НЕ удаляется: слот отработал, и второй раз запускаться он не
// должен. Иначе демон, перезапустившись в ту же минуту, запустил бы пайплайн
// заново.
func (q *queue) Release(key, outcome string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fault != nil {
		return q.fault
	}
	id, ok := q.state.Claims[key]
	if !ok {
		return fmt.Errorf("слот %q не был занят", key)
	}
	t := q.state.Tasks[id]
	if t == nil {
		return fmt.Errorf("задача слота %q отсутствует", key)
	}
	t.Status = outcome
	t.Updated = time.Now().UTC()
	return q.commit()
}

// Interrupted — слоты, застигнутые работающими, и текст оповещения по каждому.
//
// Вызывается один раз при старте демона. Слот в статусе running означает, что
// демон умер, пока работа шла. Повторять такую работу нельзя — неизвестно, что
// она уже надела, поэтому слот помечается interrupted и по нему уходит
// оповещение.
func (q *queue) Interrupted() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fault != nil {
		return nil
	}
	var out []string
	for _, t := range q.state.Tasks {
		if t.Status != slotRunning {
			continue
		}
		t.Status = slotInterrupted
		t.Updated = time.Now().UTC()
		t.Notice = "демон остановлен во время выполнения; возможно, работа уже была частично сделана"
		out = append(out, t.Job+": "+t.Notice)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	_ = q.commit()
	return out
}

// slotKey — ключ слота: пайплайн и время слота.
//
// Время слота, а не минута тика: иначе демон, перезапустившийся в ту же
// минуту, счёл бы слот уже занятым (и пропустил бы запуск) или, наоборот,
// выполнил бы его дважды. Ключ по слоту устроен так, что перезапуск в ту же
// минуту — это тот же слот.
func slotKey(pipelineFile string, slot time.Time) string {
	return fmt.Sprintf("%s|%d", pipelineFile, slot.UTC().Unix())
}

// TrimClaims — убрать старые claim'ы, чтобы state.json не рос бесконечно.
//
// Слоты старше keepClaimAge освобождаются: они всё равно не должны запускаться
// снова, а держать их вечно незачем. Возвращается число удалённого.
func (q *queue) TrimClaims(keepClaimAge time.Duration) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fault != nil {
		return 0
	}
	cutoff := time.Now().UTC().Add(-keepClaimAge)
	removed := 0
	for key, id := range q.state.Claims {
		t := q.state.Tasks[id]
		if t == nil || (t.Updated.Before(cutoff) && t.Status != slotRunning) {
			// Работающую задачу не трогаем: пока она running, демон жив и
			// слот занят. Застигнутую работу помечает interrupted — это делает
			// Interrupted() при старте, и до неё задача ещё не running.
			delete(q.state.Claims, key)
			delete(q.state.Tasks, id)
			removed++
		}
	}
	if removed > 0 {
		_ = q.commit()
	}
	return removed
}

// claimAge — сколько живут claim'ы. Больше суток: слот, застрявший во
// «running», полезно помнить и поднимать тревогу, а не забывать.
const claimAge = 24 * time.Hour
