package cli

// v0.17: registry validate — trust-гейт реестра.
// Проверяет ВСЕ записи registry.yaml: плагины (манифест, id, конформность)
// и пресеты (парсинг + валидация). Это то, что CI прогоняет до того, как
// плагин/пресет попадает в реестр: запись в registry.yaml без зелёной
// конформности — не запись, а долг.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wykserdex/fidespipe/internal/common"

	"github.com/wykserdex/fidespipe/internal/core"
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/registry"
)

// RunRegistryValidate — fidespipe registry validate [--registry=<url|path>] [--local-source=<dir>]
func RunRegistryValidate(args []string) {
	regSrc := registry.DefaultSource()
	localSource := ""
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--registry="); ok {
			regSrc = v
		}
		if v, ok := strings.CutPrefix(a, "--local-source="); ok {
			localSource = v
		}
	}
	h, err := registry.Load(regSrc)
	if err != nil {
		fmt.Println("ошибка реестра:", err)
		os.Exit(1)
	}
	defer h.Close()

	cache := map[string]*srcRoot{}
	var tmpRoots []string
	failures := 0

	// ── плагины: манифест, id, конформность ──
	pnames := h.PluginNames()
	sort.Strings(pnames)
	for _, name := range pnames {
		entry, _ := h.GetPlugin(name)
		ok := true
		detail := ""
		if err := registry.ValidateEntry(entry, true); err != nil {
			ok, detail = false, err.Error()
		}
		var root *srcRoot
		var err error
		if ok {
			root, err = resolveRoot(entry, h.Dir, localSource, cache, &tmpRoots)
		}
		if !ok {
			detail = "pin: " + detail
		} else if err != nil {
			ok, detail = false, "source: "+err.Error()
		} else {
			dir := filepath.Join(root.root, entry.Path)
			if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
				dir = filepath.Join(root.root, "plugins", name)
			}
			m, lerr := core.NewEngine().LoadManifest(dir)
			if lerr != nil {
				ok, detail = false, "манифест: "+lerr.Error()
			} else if errs := pipeline.ValidatePluginDir(dir); len(errs) > 0 {
				ok, detail = false, "манифест: "+strings.Join(errs, "; ")
			} else if m.ID != name {
				ok, detail = false, fmt.Sprintf("id в манифесте %q ≠ имя в реестре %q", m.ID, name)
			} else if _, serr := os.Stat(filepath.Join(dir, "plugin.test.yaml")); serr != nil {
				ok, detail = false, "нет plugin.test.yaml — конформность обязательна для реестра"
			} else {
				passed, failed, terr := core.RunPluginTests(dir, "", true)
				if terr != nil {
					ok, detail = false, "тесты: "+terr.Error()
				} else if passed+failed < 3 {
					ok, detail = false, fmt.Sprintf("тесты: нужно минимум 3, найдено %d", passed+failed)
				} else if failed > 0 {
					ok, detail = false, fmt.Sprintf("тесты: %d/%d PASSED", passed, passed+failed)
				} else {
					detail = fmt.Sprintf("manifest %s@%s, тесты %d PASSED", m.ID, m.Version, passed)
				}
			}
		}
		reportEntry("plugin", name, ok, detail)
		if !ok {
			failures++
		}
	}

	// ── пресеты: парсинг + валидация ──
	snames := h.PresetNames()
	sort.Strings(snames)
	for _, name := range snames {
		ok := true
		detail := ""
		raw, _, _, err := fetchPreset(name, regSrc, localSource)
		if err != nil {
			ok, detail = false, err.Error()
		} else if pf, yerr := pipeline.LoadPipelineFileFromBytes(raw); yerr != nil {
			ok, detail = false, "YAML: "+yerr.Error()
		} else {
			errs, _ := core.Validate(pf, core.NewEngine())
			if len(errs) > 0 {
				ok, detail = false, "валидация: "+strings.Join(errs, "; ")
			} else {
				detail = fmt.Sprintf("pipeline %s, %d шагов", pf.Pipeline.Name, len(pf.Pipeline.Steps))
			}
		}
		reportEntry("preset", name, ok, detail)
		if !ok {
			failures++
		}
	}

	for _, t := range tmpRoots {
		os.RemoveAll(t)
	}
	if failures > 0 {
		fmt.Printf("\n■ реестр: %d записей НЕ прошли проверку\n", failures)
		os.Exit(1)
	}
	fmt.Println("\n■ реестр: все записи проверены")
}

func reportEntry(kind, name string, ok bool, detail string) {
	mark := "  ✓"
	if !ok {
		mark = "  ✗"
	}
	fmt.Printf("%s %s/%-22s %s\n", mark, kind, name, detail)
}

// srcRoot — каталог, содержащий entry.Path (локальный source, каталог реестра или клон).
type srcRoot struct{ root string }

// resolveRoot — каталог записи без повторных клонов (кэш на (source, version)).
func resolveRoot(entry registry.Entry, hDir, localSource string, cache map[string]*srcRoot, tmpRoots *[]string) (*srcRoot, error) {
	if localSource != "" && sameRepo(entry.Source, localSource) {
		if entry.Commit != "" {
			if err := registry.VerifyCheckoutPath(localSource, entry.Commit, entry.Path); err != nil {
				return nil, fmt.Errorf("локальный source не соответствует pin: %w", err)
			}
		}
		return &srcRoot{root: localSource}, nil
	}
	key := entry.Source + "|" + entry.Version + "|" + entry.Commit
	if c, ok := cache[key]; ok {
		return c, nil
	}
	// 1) source — локальный каталог
	if fi, e := os.Stat(entry.Source); e == nil && fi.IsDir() {
		if entry.Commit != "" {
			if err := registry.VerifyCheckoutPath(entry.Source, entry.Commit, entry.Path); err != nil {
				return nil, fmt.Errorf("локальный source не соответствует pin: %w", err)
			}
		}
		c := &srcRoot{root: entry.Source}
		cache[key] = c
		return c, nil
	}
	// 2) оффлайн: каталог локального реестра и есть source
	if hDir != "" && entry.Commit == "" {
		candidate := filepath.Join(hDir, entry.Path)
		if _, e := os.Stat(candidate); e != nil {
			candidate = filepath.Join(hDir, "plugins", filepath.Base(filepath.FromSlash(entry.Path)))
		}
		if _, e := os.Stat(candidate); e == nil {
			c := &srcRoot{root: hDir}
			cache[key] = c
			return c, nil
		}
	}
	// 3) git clone (depth 1)
	tmp, err := os.MkdirTemp("", "fidespipe-registry-*")
	if err != nil {
		return nil, err
	}
	if err := registry.CloneToPinned(entry.Source, entry.Version, entry.Commit, tmp); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	*tmpRoots = append(*tmpRoots, tmp)
	c := &srcRoot{root: tmp}
	cache[key] = c
	return c, nil
}

// sameRepo — совпадает ли git origin каталога с entry.Source (для --local-source).
func sameRepo(sourceURL, dir string) bool {
	gitCmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := common.Output(gitCmd)
	if err != nil {
		return false
	}
	return normalizeRepo(string(out)) == normalizeRepo(sourceURL)
}

func normalizeRepo(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "git@")
	u = strings.Replace(u, ":", "/", 1)
	return strings.TrimSuffix(u, "/")
}
