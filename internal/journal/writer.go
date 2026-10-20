package journal

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wykserdex/fidespipe/internal/runctx"
)

const maxJournalPayloadSize = 16 << 20

// maxSnapshotPayloadSize — потолок context.json (снапшота контекста).
// Тесты понижают его, чтобы проверить отказ по размеру без 16 МБ данных —
// тот же приём, что metaScanByteBudget в reader.go.
var maxSnapshotPayloadSize = maxJournalPayloadSize

// Режимы каталога рана и файлов в нём. В каталоге лежат входы шагов и их
// выходы (journal.jsonl, context.json, artifacts/, runs.db), то есть
// содержимое данных пользователя; при 0755/0644 их читал любой локальный
// пользователь машины.
//
// Именованные константы, а не литералы в вызовах: режимы — это контракт,
// который проверяется тестом на любой платформе, тогда как фактический режим
// на диске проверяется только там, где режимы вообще действуют.
//
// На Windows они не действуют (права задаёт DACL каталога, см. SECURITY.md),
// и код обязан оставаться кроссплатформенным: MkdirAll/OpenFile с такими
// режимами на Windows — обычный вызов, ошибок он не добавляет.
const (
	runDirPerm os.FileMode = 0o700
	filePerm   os.FileMode = 0o600
)

// Journal — append-only журнал прогона: var/runs/<run_id>/journal.jsonl
type Journal struct {
	// v0.23: счётчик потерянных событий (write-ошибки)
	writeErrs int
	// P2 F-04: счётчик потерянных СНАПШОТОВ (context.json) считается
	// отдельно от потерянных событий: потерянный снапшот ломает resume
	// (состояние на диске устарело), и терминальный статус рана обязан
	// отличать её от потери пары строк журнала.
	snapshotErrs int
	// snapErr — отказ первой потери снапшота (nil, если потерь не было).
	snapErr error
	mu      sync.Mutex
	f       *os.File
	Dir     string
}

// Режимы каталога рана и журнала — см. runDirPerm/filePerm. На Windows режимы
// не действуют, и код обязан оставаться кроссплатформенным.
func NewJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, runDirPerm); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return nil, err
	}
	return &Journal{f: f, Dir: dir}, nil
}

func OpenJournalAppend(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, runDirPerm); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, filePerm)
	if err != nil {
		return nil, err
	}
	return &Journal{f: f, Dir: dir}, nil
}

// Event — journal-событие. v0.23: не мутирует переданный map (footgun для
// переиспользуемых мап), ошибки записи не глотаются (disk-full = видимая
// потеря, не молчаливая).
func (j *Journal) Event(kind string, kv map[string]interface{}) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		j.writeErrs++
		return os.ErrClosed
	}
	e := make(map[string]interface{}, len(kv)+2)
	for k, v := range kv {
		e[k] = v
	}
	e["ts"] = time.Now().UTC().Format(time.RFC3339)
	e["type"] = kind
	b, err := json.Marshal(e)
	if err != nil {
		j.writeErrs++
		fmt.Fprintf(os.Stderr, "journal: marshal %s: %v (событие потеряно)\n", kind, err)
		return err
	}
	line := append(b, '\n')
	if len(line) > maxJournalPayloadSize {
		j.writeErrs++
		err := fmt.Errorf("journal event %s exceeds %d bytes", kind, maxJournalPayloadSize)
		fmt.Fprintf(os.Stderr, "journal: %v (событие потеряно)\n", err)
		return err
	}
	n, werr := j.f.Write(line)
	if werr == nil && n != len(line) {
		werr = io.ErrShortWrite
	}
	if werr != nil {
		j.writeErrs++
		if j.writeErrs <= 3 {
			fmt.Fprintf(os.Stderr, "journal: запись %s не удалась: %v (событие потеряно)\n", kind, werr)
		}
		return werr
	}
	return nil
}

// Snapshot — context.json. v0.23: атомарно (temp+rename) — краш в середине
// больше не даёт битый файл, из-за которого resume отвалился бы.
//
// P2 F-04: потерянный снапшот больше не «ничего страшного». Ошибка
// возвращается как раньше, но потеря теперь видна и в самом журнале:
// событие snapshot_lost, счётчик SnapshotLosses() и stderr. Раньше ошибка
// уходила в пустоту — вызывающий её игнорировал, и журнал об устаревшем
// context.json умалчивал.
func (j *Journal) Snapshot(ctx *runctx.Ctx) error {
	if ctx == nil {
		return nil
	}
	reason, size, err := j.writeSnapshot(ctx)
	if err == nil {
		return nil
	}
	j.noteSnapshotLoss(reason, size, err)
	return err
}

// writeSnapshot — атомарная запись context.json под локом журнала. Возвращает
// короткую причину отказа (для журнала) и, когда он известен, размер
// несохранённого снапшота. Ни данных контекста, ни секретов в отказе нет:
// только «почему» и «сколько байт».
func (j *Journal) writeSnapshot(ctx *runctx.Ctx) (reason string, size int, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return "journal_closed", 0, os.ErrClosed
	}
	b, err := json.MarshalIndent(ctx.Data, "", "  ")
	if err != nil {
		return "serialize", 0, fmt.Errorf("context snapshot: %w", err)
	}
	if len(b) > maxSnapshotPayloadSize {
		return "too_large", len(b), fmt.Errorf("context snapshot exceeds %d bytes", maxSnapshotPayloadSize)
	}
	tmp := filepath.Join(j.Dir, "context.json.tmp")
	// temp не хуже итогового файла: он тоже содержит весь контекст рана, а
	// rename сохраняет его режим — то есть незакрытый temp стал бы открытым
	// context.json.
	if werr := os.WriteFile(tmp, b, filePerm); werr != nil {
		return "write", 0, fmt.Errorf("context snapshot: %w", werr)
	}
	if rerr := os.Rename(tmp, filepath.Join(j.Dir, "context.json")); rerr != nil {
		// temp не переименовался — он не состояние, а мусор: следующий
		// снапшот перезапишет его сам, но оставлять битый хвост нельзя.
		_ = os.Remove(tmp)
		return "rename", 0, fmt.Errorf("context snapshot rename: %w", rerr)
	}
	return "", len(b), nil
}

// noteSnapshotLoss — счётчики + событие snapshot_lost. Событие пишется на
// ПЕРВУЮ потерю: повторы того же отказа (например контекст больше потолка на
// каждом элементе) не засоряют append-only журнал, их видно по счётчику в
// терминальном событии рана. Само событие может не записаться (диск полон) —
// тогда это уже потеря события, её считает Event.
func (j *Journal) noteSnapshotLoss(reason string, size int, cause error) {
	j.mu.Lock()
	j.writeErrs++
	j.snapshotErrs++
	n := j.snapshotErrs
	if j.snapErr == nil {
		j.snapErr = cause
	}
	j.mu.Unlock()
	fmt.Fprintf(os.Stderr, "journal: снапшот контекста потерян (%s): context.json не записан\n", reason)
	if n > 1 {
		return
	}
	kv := map[string]interface{}{"reason": reason}
	if size > 0 {
		kv["bytes"] = size
	}
	_ = j.Event("snapshot_lost", kv)
}

// SnapshotLosses — сколько снапшотов контекста не записалось. Resume после
// такого рана поднимается с устаревшим состоянием, поэтому терминальный
// статус рана обязан это отражать.
func (j *Journal) SnapshotLosses() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotErrs
}

// SnapshotLossError — ошибка первой потери снапшота (nil, если потерь нет).
// Текст отказа безопасен: причина записи и размер, без данных контекста.
func (j *Journal) SnapshotLossError() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapErr
}

// WriteErrors — сколько записей не удалось (для честного финального отчёта):
// потерянные события журнала и потерянные снапшоты вместе, разложение —
// SnapshotLosses().
func (j *Journal) WriteErrors() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.writeErrs
}

func (j *Journal) Close() {
	j.mu.Lock()
	n := j.writeErrs
	snaps := j.snapshotErrs
	f := j.f
	j.f = nil
	j.mu.Unlock()
	if snaps > 0 {
		fmt.Fprintf(os.Stderr, "journal: потеряно снапшотов контекста: %d (context.json за ран неполон, --resume восстановит не всё)\n", snaps)
	}
	if n > snaps {
		fmt.Fprintf(os.Stderr, "journal: %d ошибок записи за ран (журнал неполный!)\n", n-snaps)
	}
	if f != nil {
		_ = f.Close()
	}
}
