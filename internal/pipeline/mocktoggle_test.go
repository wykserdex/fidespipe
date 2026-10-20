package pipeline

// Тестовый переключатель в permissions.secrets означает, что ядро подставит
// его значение из окружения хоста в обычный запуск. Плагин, который на такой
// переключатель смотрит, возвращает подделку вместо настоящей работы — и это
// выглядит как успех.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func manifestWithSecrets(secrets ...string) *Manifest {
	return &Manifest{
		ID: "p", Version: "0.1.0", PlatformAPI: PlatformAPI,
		Runtime: Runtime{Type: "python", Entry: "main.py"},
		Input:   map[string]Port{},
		Output:  map[string]Port{"ok": {Type: "boolean"}},
		Permissions: Permissions{
			Network: []NetworkPermission{}, Filesystem: "none", Secrets: secrets,
		},
	}
}

func TestValidateManifestRejectsTestToggleAsSecret(t *testing.T) {
	cases := []string{
		"LLM_MOCK", "MOCK", "FIDESPIPE_MOCK", "HTTP_PROBE_FAKE",
		"STUB", "DUMMY", "LLM_SIM", "PLUGIN_TEST_MODE", "SOME_STUB",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateManifest(manifestWithSecrets(name))
			if err == nil {
				t.Fatalf("тестовый переключатель %q в permissions.secrets должен отклоняться", name)
			}
			if !strings.Contains(err.Error(), "тестовый переключатель") {
				t.Errorf("ошибка должна называть суть, а не только факт отказа: %v", err)
			}
		})
	}
}

// Обратная сторона: правило не должно ловить настоящие секреты. Широкий шаблон
// вроде «всё, где есть TEST», запретил бы TESTWEBHOOK_URL — то есть отверг бы
// верный манифест, и автор пошёл бы объявлять секрет как-то иначе.
func TestValidateManifestAllowsRealSecretsWithTestInName(t *testing.T) {
	for _, name := range []string{
		"TESTWEBHOOK_URL", "MY_API_KEY", "CONTEST_TOKEN", "ATTESTATION_KEY",
		"LATEST_VERSION", "SLACK_TOKEN", "GITHUB_TOKEN",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateManifest(manifestWithSecrets(name)); err != nil {
				t.Errorf("настоящий секрет %q отклонён: %v", name, err)
			}
		})
	}
}

// Проверка обязана ловить нарушение на настоящих манифестах, а не только на
// синтетике: LLM_MOCK был объявлен в secrets всех трёх LLM-плагинов, и пока
// правила не было, валидатор был зелёным.
func TestRealManifestsDeclareNoTestToggles(t *testing.T) {
	repo, err := osFindRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(repo, "plugins", "*", "*", "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Skip("плагины не найдены")
	}
	bad := 0
	for _, path := range matches {
		for _, e := range ValidatePluginDir(filepath.Dir(path)) {
			if strings.Contains(e, "тестовый переключатель") {
				t.Errorf("настоящий манифест нарушает правило: %s: %s", path, e)
				bad++
			}
		}
	}
	if bad == 0 {
		t.Logf("проверено манифестов: %d, нарушений нет", len(matches))
	}
}

func osFindRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
