package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestQueueClaimIsIdempotent — слот, однажды занятый, не запускается второй раз.
//
// Это и есть смысл очереди: демон, перезапустившийся, пока шёл ран, не
// запустит тот же слот заново.
func TestQueueClaimIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	q, err := openQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := slotKey("p.yaml", time.Unix(1700000000, 0))
	ok, err := q.Claim(key, "p.yaml")
	if err != nil || !ok {
		t.Fatalf("первый claim: ok=%v err=%v", ok, err)
	}
	ok, err = q.Claim(key, "p.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("слот занят повторно — значит, он выполнится дважды")
	}
}

// TestQueueSurvivesRestart — очередь переживает перезапуск демона.
func TestQueueSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := openQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := slotKey("p.yaml", time.Unix(1700000000, 0))
	if _, err := q.Claim(key, "p.yaml"); err != nil {
		t.Fatal(err)
	}
	q2, err := openQueue(dir)
	if err != nil {
		t.Fatalf("очередь не пережила перезапуск: %v", err)
	}
	ok, err := q2.Claim(key, "p.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("после перезапуска слот снова занят — он выполнится дважды")
	}
}

// TestQueueMarksInterruptedSlot — работа, застигнутая при смерти демона, не
// перезапускается, а по ней уходит оповещение.
func TestQueueMarksInterruptedSlot(t *testing.T) {
	dir := t.TempDir()
	q, err := openQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := slotKey("p.yaml", time.Unix(1700000000, 0))
	if _, err := q.Claim(key, "p.yaml"); err != nil {
		t.Fatal(err)
	}
	// Демон «умер» — состояние осталось в статусе running.
	q2, err := openQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	notices := q2.Interrupted()
	if len(notices) != 1 {
		t.Fatalf("оповещений %d, ожидалось 1: %v", len(notices), notices)
	}
	// И слот больше не выполняется.
	ok, err := q2.Claim(key, "p.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("прерванный слот снова занят — работа повторится, а исход неизвестен")
	}
}

// TestQueueRejectsCorruptState — битое состояние это ошибка, а не повод
// начать с нуля и молча отработать слоты повторно.
func TestQueueRejectsCorruptState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "queue.json"), []byte("{не json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openQueue(dir); err == nil {
		t.Error("битое состояние принято — очередь молча потеряла бы историю слотов")
	}
}

// TestQueueTrimsOldClaims — старые claim'ы убираются, иначе state.json растёт
// бесконечно.
func TestQueueTrimsOldClaims(t *testing.T) {
	dir := t.TempDir()
	q, err := openQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := slotKey("p.yaml", time.Unix(1700000000, 0))
	if _, err := q.Claim(key, "p.yaml"); err != nil {
		t.Fatal(err)
	}
	if n := q.TrimClaims(time.Millisecond); n != 1 {
		t.Errorf("удалено %d claim'ов, ожидался 1", n)
	}
}
