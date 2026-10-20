package journal

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultRunsDir — каталог прогонов по умолчанию (см. DefaultRunsDirAt).
func DefaultRunsDir() string {
	return DefaultRunsDirAt("")
}

// DefaultRunsDirAt — каталог прогонов по умолчанию относительно base: это
// base/var/runs, а если такого каталога нет — base/runs. Вне репозитория Fidespipe
// каталога var/ обычно не существует, и без фолбэка ран писался в runs/, пока
// команды чтения искали строго var/runs.
//
// Правило живёт здесь, а не в каждой команде CLI: `pipeline run` знал про
// фолбэк, а `runs list`/`runs show` — нет, и только что записанный ран был не
// виден для чтения. Кто пишет, тот и читает — из одного места. base="" —
// текущий каталог.
func DefaultRunsDirAt(base string) string {
	primary := filepath.Join(base, "var", "runs")
	if info, err := os.Stat(primary); err == nil && info.IsDir() {
		return primary
	}
	return filepath.Join(base, "runs")
}

type RunStore interface {
	Create(runID string) (*Journal, error)
	OpenAppend(runID string) (*Journal, error)
	SaveArtifact(runID string, name string, data []byte) error
	LoadContext(runID string) (map[string]interface{}, error)
	MaxItemIndex(runID string) (int, error)
	ListArtifacts(runID string) ([]string, error)
	LoadArtifact(runID string, name string) ([]byte, error)
	ListRuns() ([]string, error)
}

type FilesystemStore struct {
	BaseDir string
}

func NewFilesystemStore(baseDir string) *FilesystemStore {
	if baseDir == "" {
		baseDir = DefaultRunsDir()
	}
	return &FilesystemStore{BaseDir: baseDir}
}

func ValidateRunID(runID string) error {
	if runID == "" || len(runID) > 160 {
		return fmt.Errorf("небезопасный run_id %q", runID)
	}
	for _, r := range runID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("небезопасный run_id %q", runID)
	}
	return nil
}

func resolveJournalPath(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	suffix := []string{}
	for {
		resolved, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for _, part := range suffix {
				resolved = filepath.Join(resolved, part)
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(evalErr) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", evalErr
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}

func SafeRunDir(baseDir, runID string) (string, error) {
	if err := ValidateRunID(runID); err != nil {
		return "", err
	}
	if baseDir == "" {
		baseDir = DefaultRunsDir()
	}
	root, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, runID)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("небезопасный run_id %q", runID)
	}
	rootReal, err := resolveJournalPath(root)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Lstat(dir); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("run_dir является symlink")
		}
		resolved, resolveErr := resolveJournalPath(dir)
		if resolveErr != nil {
			return "", resolveErr
		}
		resolvedRel, relErr := filepath.Rel(rootReal, resolved)
		if relErr != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("run_dir уходит через symlink")
		}
		return dir, nil
	} else if !os.IsNotExist(statErr) {
		return "", statErr
	}
	resolved, err := resolveJournalPath(dir)
	if err != nil {
		return "", err
	}
	resolvedRel, err := filepath.Rel(rootReal, resolved)
	if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("родительский run_dir уходит через symlink")
	}
	return dir, nil
}

func (s *FilesystemStore) runDir(runID string) (string, error) {
	return SafeRunDir(s.BaseDir, runID)
}

func (s *FilesystemStore) Create(runID string) (*Journal, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return nil, err
	}
	return NewJournal(dir)
}

func (s *FilesystemStore) OpenAppend(runID string) (*Journal, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return nil, err
	}
	return OpenJournalAppend(dir)
}

func (s *FilesystemStore) SaveArtifact(runID string, name string, data []byte) error {
	runDir, err := s.runDir(runID)
	if err != nil {
		return err
	}
	dir := filepath.Join(runDir, "artifacts")
	// Режимы — как у каталога рана и файлов в нём (runDirPerm/filePerm).
	if err := os.MkdirAll(dir, runDirPerm); err != nil {
		return err
	}
	clean := filepath.Base(name)
	return os.WriteFile(filepath.Join(dir, clean), data, filePerm)
}

func (s *FilesystemStore) LoadContext(runID string) (map[string]interface{}, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "context.json"))
	if err != nil {
		return nil, fmt.Errorf("context.json: %w", err)
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("context.json битый: %w", err)
	}
	return data, nil
}

func ParseItemIndex(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		maxInt := int64(^uint(0) >> 1)
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n < 0 || n >= float64(maxInt) {
			return 0, false
		}
		i := int(n)
		if i < 0 || float64(i) != n {
			return 0, false
		}
		return i, true
	case float32:
		return ParseItemIndex(float64(n))
	case int:
		if n < 0 {
			return 0, false
		}
		return n, true
	case int8:
		if n < 0 {
			return 0, false
		}
		return int(n), true
	case int16:
		if n < 0 {
			return 0, false
		}
		return int(n), true
	case int32:
		if n < 0 {
			return 0, false
		}
		return int(n), true
	case int64:
		if n < 0 || int64(int(n)) != n {
			return 0, false
		}
		return int(n), true
	case uint:
		if uint64(n) > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(n), true
	case uint8:
		return int(n), true
	case uint16:
		return int(n), true
	case uint32:
		if uint64(n) > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(n), true
	case uint64:
		if n > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 || int64(int(i)) != i {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

// P2 F-03: max index нужен по ВСЕМ item_end, поэтому Events() (полное
// чтение в память) здесь не годится — потоковый проход ScanMeta держит O(1)
// памяти и читает те же поля журнала.
func (s *FilesystemStore) MaxItemIndex(runID string) (int, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return -1, err
	}
	maxIdx := -1
	_, err = NewReader(dir).ScanMeta(func(meta EventMeta) error {
		if meta.Type != "item_end" {
			return nil
		}
		if meta.Status != "" && meta.Status != "ok" {
			return nil
		}
		if meta.ItemIndex == nil {
			return nil
		}
		if idx, ok := ParseItemIndex(*meta.ItemIndex); ok && idx > maxIdx {
			maxIdx = idx
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return -1, nil
		}
		return -1, err
	}
	return maxIdx, nil
}

func (s *FilesystemStore) Load(runID string) (map[string]interface{}, error) {
	return s.LoadContext(runID)
}

func (s *FilesystemStore) ListArtifacts(runID string) ([]string, error) {
	runDir, err := s.runDir(runID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(runDir, "artifacts")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (s *FilesystemStore) LoadArtifact(runID string, name string) ([]byte, error) {
	runDir, err := s.runDir(runID)
	if err != nil {
		return nil, err
	}
	clean := filepath.Base(name)
	p := filepath.Join(runDir, "artifacts", clean)
	return os.ReadFile(p)
}

func (s *FilesystemStore) ListRuns() ([]string, error) {
	ents, err := os.ReadDir(s.BaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	out := []string{}
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// JsonStore — индекс прогонов и артефактов в одном JSON-файле (var/runs/runs.db).
// Журнал (journal.jsonl) остаётся единственным источником событий; store не
// индексирует каждое событие, поэтому не создаёт O(n²) rewrite на append.

type dbFile struct {
	Runs      []dbRun      `json:"runs"`
	Events    []dbEvent    `json:"events"`
	Artifacts []dbArtifact `json:"artifacts"`
}

type dbRun struct {
	ID string `json:"id"`
}

type dbEvent struct {
	ID        int                    `json:"id"`
	RunID     string                 `json:"run_id"`
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data"`
	ItemIndex *int                   `json:"item_index,omitempty"`
}

type dbArtifact struct {
	ID    int    `json:"id"`
	RunID string `json:"run_id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
}

type JsonStore struct {
	FilesystemStore
	DBPath string
	mu     sync.Mutex
}

func NewJsonStore(baseDir, dbPath string) *JsonStore {
	if baseDir == "" {
		baseDir = DefaultRunsDir()
	}
	if dbPath == "" {
		dbPath = filepath.Join(baseDir, "runs.db")
	}
	return &JsonStore{
		FilesystemStore: FilesystemStore{BaseDir: baseDir},
		DBPath:          dbPath,
	}
}

func (s *JsonStore) loadDB() (*dbFile, error) {
	if _, err := os.Stat(s.DBPath); os.IsNotExist(err) {
		return &dbFile{}, nil
	}
	raw, err := os.ReadFile(s.DBPath)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return &dbFile{}, nil
	}
	var db dbFile
	if err := json.Unmarshal(raw, &db); err != nil {
		return nil, fmt.Errorf("индекс %s не читается как JSON: %w", s.DBPath, err)
	}
	return &db, nil
}

func (s *JsonStore) saveDB(db *dbFile) error {
	// Индекс ранов перечисляет выходы шагов, поэтому закрыт так же, как они
	// сами (runDirPerm/filePerm).
	if err := os.MkdirAll(filepath.Dir(s.DBPath), runDirPerm); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.DBPath, raw, filePerm)
}

func (s *JsonStore) ensureRun(runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.loadDB()
	if err != nil {
		return err
	}
	for _, r := range db.Runs {
		if r.ID == runID {
			return nil
		}
	}
	db.Runs = append(db.Runs, dbRun{ID: runID})
	return s.saveDB(db)
}

func (s *JsonStore) Create(runID string) (*Journal, error) {
	j, err := s.FilesystemStore.Create(runID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	db, err := s.loadDB()
	if err != nil {
		s.mu.Unlock()
		j.Close()
		return nil, err
	}
	exists := false
	for _, r := range db.Runs {
		if r.ID == runID {
			exists = true
			break
		}
	}
	if !exists {
		db.Runs = append(db.Runs, dbRun{ID: runID})
		if err := s.saveDB(db); err != nil {
			s.mu.Unlock()
			j.Close()
			return nil, err
		}
	}
	s.mu.Unlock()
	return j, nil
}

func (s *JsonStore) OpenAppend(runID string) (*Journal, error) {
	j, err := s.FilesystemStore.OpenAppend(runID)
	if err != nil {
		return nil, err
	}
	if err := s.ensureRun(runID); err != nil {
		j.Close()
		return nil, err
	}
	return j, nil
}

func (s *JsonStore) SaveArtifact(runID string, name string, data []byte) error {
	if err := s.FilesystemStore.SaveArtifact(runID, name, data); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.loadDB()
	if err != nil {
		return err
	}
	clean := filepath.Base(name)
	runDir, err := s.runDir(runID)
	if err != nil {
		return err
	}
	p := filepath.Join(runDir, "artifacts", clean)
	db.Artifacts = append(db.Artifacts, dbArtifact{
		ID:    len(db.Artifacts) + 1,
		RunID: runID,
		Name:  clean,
		Path:  p,
	})
	return s.saveDB(db)
}

func (s *JsonStore) MaxItemIndex(runID string) (int, error) {
	runDir, err := s.runDir(runID)
	if err != nil {
		return -1, err
	}
	journalPath := filepath.Join(runDir, "journal.jsonl")
	if _, err := os.Stat(journalPath); err == nil {
		return s.FilesystemStore.MaxItemIndex(runID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.loadDB()
	if err != nil {
		return -1, err
	}
	maxIdx := -1
	for _, ev := range db.Events {
		if ev.RunID != runID || ev.Type != "item_end" {
			continue
		}
		if status, ok := ev.Data["status"].(string); ok && status != "ok" {
			continue
		}
		if ev.ItemIndex != nil && *ev.ItemIndex > maxIdx {
			maxIdx = *ev.ItemIndex
			continue
		}
		if v, ok := ev.Data["item_index"]; ok {
			if idx, ok := ParseItemIndex(v); ok && idx > maxIdx {
				maxIdx = idx
			}
		}
	}
	return maxIdx, nil
}

func (s *JsonStore) ListArtifacts(runID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.loadDB()
	if err != nil {
		return s.FilesystemStore.ListArtifacts(runID)
	}
	out := []string{}
	for _, a := range db.Artifacts {
		if a.RunID == runID {
			out = append(out, a.Name)
		}
	}
	if len(out) == 0 {
		return s.FilesystemStore.ListArtifacts(runID)
	}
	return out, nil
}

func (s *JsonStore) LoadArtifact(runID string, name string) ([]byte, error) {
	return s.FilesystemStore.LoadArtifact(runID, name)
}

func (s *JsonStore) ListRuns() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.loadDB()
	if err != nil {
		return s.FilesystemStore.ListRuns()
	}
	if len(db.Runs) == 0 {
		return s.FilesystemStore.ListRuns()
	}
	out := []string{}
	for _, r := range db.Runs {
		out = append(out, r.ID)
	}
	return out, nil
}

func (s *JsonStore) Close() error { return nil }
