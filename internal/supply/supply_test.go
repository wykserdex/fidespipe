package supply

// Тесты проверки цепочки поставки. Ключевой сценарий — расхождение: именно
// его шаг ловит, поэтому тест без расхождения ничего бы не проверял.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodPin = "3d3c42e5aac5ba805825da76410c181273ba90b1"

// repoFixture — синтетический репозиторий с одним воркфлоу. Собирается на
// каждый тест, чтобы правки одного случая не ломали другой.
type repoFixture struct {
	dir string
}

func newRepo(t *testing.T) *repoFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &repoFixture{dir: dir}
}

func (f *repoFixture) write(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(f.dir, ".github", "workflows", name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// goodRelease — релизный воркфлоу, который проходит все проверки.
const goodRelease = `name: release
on:
  workflow_call:
permissions:
  contents: write
  id-token: write
  attestations: write
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@` + goodPin + ` # v7.0.1
      - uses: sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6 # v4.1.2
        with:
          cosign-release: 'v3.1.3'
      - name: sign
        run: |
          set -eu
          command -v cosign >/dev/null 2>&1 || { echo "нет cosign" >&2; exit 1; }
          for f in dist/SHA256SUMS dist/sbom.spdx.json; do
            cosign sign-blob "$f" --bundle "$f.sigstore.json" --yes
          done
      - name: verify
        run: |
          set -eu
          for f in dist/SHA256SUMS dist/sbom.spdx.json; do
            cosign verify-blob "$f" --bundle "$f.sigstore.json"
          done
`

func TestPinnedActionsPass(t *testing.T) {
	f := newRepo(t)
	f.write(t, "release.yml", goodRelease)
	out, err := CheckRepo(f.dir)
	if err != nil {
		t.Fatalf("хороший воркфлоу не прошёл: %v", err)
	}
	if !strings.Contains(out, "SHA256SUMS") {
		t.Errorf("сводка не называет подписываемый файл: %q", out)
	}
}

func TestUnpinnedActionFails(t *testing.T) {
	for _, tc := range []struct{ name, uses string }{
		{"тег", "actions/checkout@v4"},
		{"ветка", "actions/checkout@main"},
		{"без рефа", "actions/checkout"},
		{"обрезанный sha", "actions/checkout@" + goodPin[:12]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepo(t)
			f.write(t, "release.yml", strings.Replace(
				goodRelease, "actions/checkout@"+goodPin, tc.uses, 1))
			_, err := CheckRepo(f.dir)
			if err == nil {
				t.Fatalf("действие %q принято как закреплённое", tc.uses)
			}
			if !strings.Contains(err.Error(), tc.uses) {
				t.Errorf("в ошибке нет самого значения: %v", err)
			}
		})
	}
}

func TestLocalReusableWorkflowAllowed(t *testing.T) {
	f := newRepo(t)
	f.write(t, "release.yml", goodRelease)
	f.write(t, "ci.yml", "name: ci\njobs:\n  release:\n    uses: ./.github/workflows/release.yml\n")
	if _, err := CheckRepo(f.dir); err != nil {
		t.Fatalf("локальный переиспользуемый воркфлоу не запрещён: %v", err)
	}
}

func TestUnsignedReleaseFails(t *testing.T) {
	cases := map[string]func(string) string{
		"подписи нет": func(s string) string {
			return strings.Replace(s, "cosign sign-blob", "sign-something", 1)
		},
		"проверки подписи нет": func(s string) string {
			return strings.Replace(s, "cosign verify-blob", "verify-something", 1)
		},
		"инсталлера cosign нет": func(s string) string {
			return strings.Replace(s, "sigstore/cosign-installer@", "other/cosign-installer@", 1)
		},
		"охраны command -v нет": func(s string) string {
			return strings.Replace(s, "command -v cosign", "true", 1)
		},
		"SHA256SUMS не подписан": func(s string) string {
			return strings.ReplaceAll(s, "dist/SHA256SUMS dist/sbom.spdx.json",
				"dist/sbom.spdx.json")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRepo(t)
			f.write(t, "release.yml", mutate(goodRelease))
			if _, err := CheckRepo(f.dir); err == nil {
				t.Fatal("расхождение не поймано")
			}
		})
	}
}

func TestNoWorkflowsFails(t *testing.T) {
	f := newRepo(t)
	if _, err := CheckRepo(f.dir); err == nil {
		t.Fatal("пустой каталог воркфлоу прошёл как норма")
	}
}

// Реальный репозиторий: шаг `supply` в `fidespipe check` полезен ровно настолько,
// насколько он проходит здесь. Синтетическая фикстура выше проверила бы саму
// себя.
func TestRealRepoPasses(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := wd
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		root = filepath.Dir(root)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("не найден корень репозитория")
	}
	if _, err := CheckRepo(root); err != nil {
		t.Fatalf("репозиторий не проходит собственную проверку: %v", err)
	}
}
