package api

import (
	"path/filepath"
	"testing"

	"github.com/wykserdex/fidespipe/internal/journal"
)

func TestCachedRunSummaryReusesUnchangedJournal(t *testing.T) {
	dir := t.TempDir()
	j, err := journal.NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.Event("run_start", map[string]interface{}{"pipeline": "demo"})
	j.Event("run_end", map[string]interface{}{"ok": 1, "aborted": 0})
	j.Close()
	s := NewServer("", "", filepath.Dir(dir))
	first := s.cachedRunSummary(dir)
	first["status"] = "mutated"
	second := s.cachedRunSummary(dir)
	if second["status"] != "ok" {
		t.Fatalf("cache was mutated: %v", second)
	}
}

func TestSummarizeRunResumedIsRunning(t *testing.T) {
	summary := summarizeRun("run", []map[string]interface{}{
		{"type": "run_end", "aborted": float64(0)},
		{"type": "run_resumed"},
	})
	if summary["status"] != "running" {
		t.Fatalf("status=%v", summary["status"])
	}
}
