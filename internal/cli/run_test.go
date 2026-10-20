package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/execution"
	"github.com/wykserdex/fidespipe/internal/registry"
)

func TestRunIDFromDir(t *testing.T) {
	if got := runIDFromDir("var/runs/20260924-120000-demo"); got != "20260924-120000-demo" {
		t.Fatalf("run id = %q", got)
	}
	if got := runIDFromDir(""); got != "" {
		t.Fatalf("empty run id = %q", got)
	}
}

// PROTOCOL §6: 0 — ран дошёл до конца; 1 — рановая неудача (в одиночном
// режиме stop/reject). В батче (foreach) частичные aborts — per-item итог
// в журнале, а не рановая неудача.
func TestRunExitCode(t *testing.T) {
	cases := []struct {
		name    string
		foreach string
		stats   execution.RunStats
		want    int
	}{
		{"одиночный ран успешен", "", execution.RunStats{OK: 1}, 0},
		{"одиночный ран упал (stop/reject)", "", execution.RunStats{Aborted: 1}, 1},
		{"одиночный ран без выполненных шагов", "", execution.RunStats{}, 1},
		{"батч дошёл до конца с частичными abort", "input.values", execution.RunStats{OK: 2, Aborted: 1}, 0},
		{"батч целиком из битых элементов", "input.values", execution.RunStats{Aborted: 3}, 0},
	}
	for _, c := range cases {
		if got := runExitCode(c.foreach, c.stats); got != c.want {
			t.Errorf("%s: runExitCode(%q, %+v)=%d, want %d", c.name, c.foreach, c.stats, got, c.want)
		}
	}
}

func runRegistryTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func registryTestSource(t *testing.T) (source, pinned, checkout string) {
	t.Helper()
	checkout = t.TempDir()
	runRegistryTestGit(t, checkout, "init", "-q", "-b", "main")
	runRegistryTestGit(t, checkout, "config", "user.email", "registry@test")
	runRegistryTestGit(t, checkout, "config", "user.name", "registry")
	runRegistryTestGit(t, checkout, "config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.MkdirAll(filepath.Join(checkout, "plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(checkout, "plugin", "marker.txt")
	if err := os.WriteFile(marker, []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRegistryTestGit(t, checkout, "add", ".")
	runRegistryTestGit(t, checkout, "commit", "-q", "-m", "A")
	pinned = runRegistryTestGit(t, checkout, "rev-parse", "HEAD")
	if err := os.WriteFile(marker, []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRegistryTestGit(t, checkout, "add", ".")
	runRegistryTestGit(t, checkout, "commit", "-q", "-m", "B")
	source = "file://" + checkout
	runRegistryTestGit(t, checkout, "remote", "add", "origin", source)
	return source, pinned, checkout
}

func TestResolveRootDoesNotUseRegistryCheckoutForRemotePin(t *testing.T) {
	source, pinned, checkout := registryTestSource(t)
	entry := registry.Entry{Source: source, Path: "plugin", Version: "main", Commit: pinned}
	var roots []string
	root, err := resolveRoot(entry, checkout, "", map[string]*srcRoot{}, &roots)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, dir := range roots {
			os.RemoveAll(dir)
		}
	}()
	if filepath.Clean(root.root) == filepath.Clean(checkout) {
		t.Fatal("remote pin was resolved from the registry checkout")
	}
	if err := registry.VerifyCheckoutCommit(root.root, pinned); err != nil {
		t.Fatalf("resolved root does not match pin: %v", err)
	}
}

func TestResolveRootRejectsMismatchedExplicitLocalSource(t *testing.T) {
	source, pinned, checkout := registryTestSource(t)
	entry := registry.Entry{Source: source, Path: "plugin", Version: "main", Commit: pinned}
	var roots []string
	_, err := resolveRoot(entry, checkout, checkout, map[string]*srcRoot{}, &roots)
	if err == nil {
		t.Fatal("mismatched explicit local source was accepted")
	}
}

func TestResolveRootAcceptsMatchingExplicitLocalSource(t *testing.T) {
	source, pinned, checkout := registryTestSource(t)
	runRegistryTestGit(t, checkout, "checkout", "-q", pinned)
	entry := registry.Entry{Source: source, Path: "plugin", Version: "main", Commit: pinned}
	var roots []string
	root, err := resolveRoot(entry, checkout, checkout, map[string]*srcRoot{}, &roots)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(root.root) != filepath.Clean(checkout) {
		t.Fatalf("root = %q, want %q", root.root, checkout)
	}
	if len(roots) != 0 {
		t.Fatalf("unexpected temporary roots: %v", roots)
	}
}
