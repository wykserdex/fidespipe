package registry

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Lock — метка установленного плагина (.wedra в каталоге плагина).
// Нужен для пиннинга: пайплайн может требовать name@version, раннер сверяет.
type Lock struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Path        string `json:"path"`
	Version     string `json:"version"`
	InstalledAt string `json:"installed_at"`
	// DonorPath — абсолютный путь к поставленному донору (пусто = донора нет).
	//
	// Пишется ПОСЛЕ установки донора, отдельной записью: на момент публикации
	// каталога донора ещё нет, и дописывать его в lock на ходу нельзя — файл
	// метки уже лежит на диске, и переписывание после факта выглядит подменой.
	DonorPath string `json:"donor_path,omitempty"`
}

const LockFile = ".wedra"

func WriteLock(dir string, l Lock) error {
	l.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(&l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, LockFile), raw, 0o644)
}

func ReadLock(dir string) (Lock, error) {
	var l Lock
	raw, err := os.ReadFile(filepath.Join(dir, LockFile))
	if err != nil {
		return l, err
	}
	err = json.Unmarshal(raw, &l)
	return l, err
}

func InstalledVersion(dir string) (string, bool) {
	l, err := ReadLock(dir)
	if err != nil || l.Version == "" {
		return "", false
	}
	return l.Version, true
}

func ValidateCommit(commit string) error {
	if commit == "" {
		return nil
	}
	if len(commit) != 40 && len(commit) != 64 {
		return fmt.Errorf("commit должен быть полным SHA (40 или 64 hex), got %q", commit)
	}
	if _, err := hex.DecodeString(commit); err != nil {
		return fmt.Errorf("commit не hex: %q", commit)
	}
	return nil
}

func VerifyCheckoutPath(dir, commit, path string) error {
	if err := ValidateCommit(commit); err != nil {
		return err
	}
	ancestorOut, err := runGit("-C", dir, "merge-base", "--is-ancestor", commit, "HEAD")
	if err != nil {
		return fmt.Errorf("pin %s не является предком HEAD: %s: %s", commit, err, strings.TrimSpace(string(ancestorOut)))
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == "" {
		clean = "."
	}
	pinned, err := runGit("-C", dir, "rev-parse", commit+":"+clean)
	if err != nil {
		return fmt.Errorf("pin %s не содержит %s: %s", commit, clean, strings.TrimSpace(string(pinned)))
	}
	current, err := runGit("-C", dir, "rev-parse", "HEAD:"+clean)
	if err != nil {
		return fmt.Errorf("HEAD не содержит %s: %s", clean, strings.TrimSpace(string(current)))
	}
	if strings.TrimSpace(string(pinned)) != strings.TrimSpace(string(current)) {
		return fmt.Errorf("локальный %s отличается от pin %s", clean, commit)
	}
	status, err := runGit("-C", dir, "status", "--porcelain", "--untracked-files=all", "--", clean)
	if err != nil {
		return fmt.Errorf("не удалось проверить локальное состояние %s: %s", clean, err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("локальный %s содержит незакоммиченные изменения", clean)
	}
	return nil
}

func VerifyCheckoutCommit(dir, commit string) error {
	if err := ValidateCommit(commit); err != nil {
		return err
	}
	out, err := runGit("-C", dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("не удалось прочитать HEAD %s: %s: %s", dir, err, strings.TrimSpace(string(out)))
	}
	got := strings.TrimSpace(string(out))
	if !strings.EqualFold(got, commit) {
		return fmt.Errorf("HEAD %s не совпадает с pin %s", got, commit)
	}
	return nil
}

// CloneTo — git clone depth-1 source@ref → dir (dir создаётся).
// v0.28a: pinned-вариант с проверкой commit — см. CloneToPinned.
func CloneTo(source, ref, dir string) error {
	return CloneToPinned(source, ref, "", dir)
}

// CloneToPinned clones the requested source revision and applies the registry commit pin.
//   - commit == "" — классика: клон ref (тег/ветка);
//   - commit != "" — детерминизм: тянем РОВНО этот SHA (git fetch <sha>),
//     тег из реестра в дело не вмешивается. Тег переставили — всё равно
//     ставим зафиксированный контент; SHA переписали (полный захват репо) —
//     fetch не найдёт коммит и установка падает (fail-closed).
//
// SHA не может «указывать на себя» (он неизвестен до коммита), поэтому пин —
// всегда SHA предшествующей проверенной ревизии, содержимое которой сверено.
func CloneToPinned(source, ref, commit, dir string) error {
	if err := ValidateCommit(commit); err != nil {
		return err
	}
	if commit == "" {
		return cloneRef(source, ref, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	steps := [][]string{
		{"init", "-q", dir},
		// Keep checkout line endings stable across platforms.
		{"-C", dir, "config", "core.autocrlf", "false"},
		{"-C", dir, "remote", "add", "origin", source},
		{"-C", dir, "fetch", "-q", "--depth", "1", "origin", commit},
		{"-C", dir, "checkout", "-q", "-f", "FETCH_HEAD"},
	}
	for _, a := range steps {
		out, err := runGit(a...)
		if err != nil {
			return fmt.Errorf("supply-chain пин %s@%s: %s: %s", source, commit, err, string(out))
		}
	}
	return VerifyCheckoutCommit(dir, commit)
}

func cloneRef(source, ref, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	out, err := runGit("-c", "core.autocrlf=false",
		"clone", "--depth", "1", "--branch", ref, "--quiet", "--", source, dir)
	if err != nil {
		return fmt.Errorf("git clone %s@%s: %s: %s", source, ref, err, string(out))
	}
	return nil
}

// CopyDir — рекурсивное копирование (без скрытых файлов исходника,
// кроме .gitignore ничего не тащим; .wedra из клона не тащим — свой пишем).
func CopyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink в plugin source запрещён: %s", path)
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == ".wedra" {
				return filepath.SkipDir
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			mode := info.Mode().Perm()
			if mode == 0 {
				mode = 0o755
			}
			if path == src {
				return os.MkdirAll(dst, mode)
			}
			return os.MkdirAll(filepath.Join(dst, stringsRel(src, path)), mode)
		}
		base := filepath.Base(path)
		if base == ".wedra" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		return os.WriteFile(filepath.Join(dst, stringsRel(src, path)), data, mode)
	})
}

func stringsRel(base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return filepath.Base(p)
	}
	return rel
}
