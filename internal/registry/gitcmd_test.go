package registry

// Гигиена вызовов git: пользовательское окружение не должно решать, что
// клонируется и по какому протоколу.
//
// Проверяются две вещи, и обе обязаны были сломаться до правки:
//
//	(а) состав команды и окружения — то, что читается без сети;
//	(б) реальный клон по file:// — то, что сломается, если запретить протокол
//	    или занулить конфиг не тем путём.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/common"
)

func envValue(env []string, key string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return strings.TrimPrefix(kv, key+"=")
		}
	}
	return ""
}

// Протоколы: ничего не разрешено, кроме названных явно.
func TestGitCmdRestrictsProtocols(t *testing.T) {
	args := gitCmd("clone", "x", "y").Args
	want := map[string]bool{
		"protocol.allow=never":        false,
		"protocol.https.allow=always": false,
		"protocol.ssh.allow=always":   false,
		"protocol.file.allow=always":  false,
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" {
			if _, ok := want[args[i+1]]; ok {
				want[args[i+1]] = true
			}
		}
	}
	for setting, seen := range want {
		if !seen {
			t.Errorf("в команде git нет -c %s: %v", setting, args)
		}
	}
	// exec:: и file:: без явного разрешения не поднимаются — именно тот класс
	// (CVE-2022-39253), ради которого список и замыкается вручную.
	for _, arg := range args {
		if strings.HasPrefix(arg, "protocol.ext.") {
			t.Errorf("протокол ext не должен разрешаться: %v", args)
		}
	}
	if args[len(args)-3] != "clone" {
		t.Fatalf("префикс -c должен стоять ДО подкоманды, иначе git его не прочтёт: %v", args)
	}
}

// Окружение: глобальный конфиг выключен, интерактивный промпт выключен.
func TestGitCmdNeutralisesUserEnvironment(t *testing.T) {
	env := gitCmd("status").Env
	if got := envValue(env, "GIT_CONFIG_GLOBAL"); got != os.DevNull {
		t.Fatalf("GIT_CONFIG_GLOBAL = %q, ждали %q (на Windows это NUL, а не /dev/null)", got, os.DevNull)
	}
	if got := envValue(env, "GIT_TERMINAL_PROMPT"); got != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT = %q, ждали 0 — иначе клон может зависнуть на пароле", got)
	}
	// Окружение не выбрасывается: PATH и прочее остаются, иначе git не найдётся.
	if envValue(env, "PATH") == "" && os.Getenv("PATH") != "" {
		t.Fatal("PATH потерялся: команда git станет ненайденной")
	}
}

// Клон по file:// обязан продолжать работать: запрет протоколов и нулевой
// глобальный конфиг не должны ломать ни установку из локального репозитория,
// ни существующие тесты пинов.
func TestPinnedCloneStillWorksWithHardenedGit(t *testing.T) {
	src, shaA, _ := pinRepo(t, false)
	dst := filepath.Join(t.TempDir(), "plug")
	if err := CloneToPinned(src, "v1", shaA, dst); err != nil {
		t.Fatalf("пинованный клон с усиленным окружением не прошёл: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dst, "f.txt"))
	if err != nil || string(raw) != "A\n" {
		t.Fatalf("содержимое клона: %q err=%v (want A)", raw, err)
	}
	if err := VerifyCheckoutCommit(dst, shaA); err != nil {
		t.Fatalf("VerifyCheckoutCommit с усиленным окружением: %v", err)
	}
}

func TestLegacyCloneStillWorksWithHardenedGit(t *testing.T) {
	src, _, _ := pinRepo(t, false)
	dst := filepath.Join(t.TempDir(), "plug")
	if err := CloneTo(src, "v1", dst); err != nil {
		t.Fatalf("кло�� по тегу с усиленным окружением не прошёл: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "f.txt")); err != nil {
		t.Fatalf("в клоне нет файла: %v", err)
	}
}

// Транспорт ext:: выполняет произвольную команду (CVE-2022-39253). Проверка
// не «флаги в аргументах», а результат: команда обязана не выполниться, и
// метка рядом с temp-каталогом — не появиться.
//
// На Windows ext:: и без того не работает (нет sh), поэтому проверять там
// нечего и тест был бы вакуумно-зелёным.
func TestGitCmdRefusesExtTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ext:: опирается на sh, которого на Windows нет")
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := runGit("clone", "--depth", "1", "--quiet", "--", "ext::sh -c touch "+marker, dst); err == nil {
		t.Fatal("транспорт ext:: принят — это исполнение произвольной команды от имени git")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("команда из ext:: выполнилась: метка %s создана", marker)
	}
}

// Список протоколов не «написан и забыт», а работает: тот же клон по file://
// с тем же protocol.allow=never, но БЕЗ protocol.file.allow обязан упасть.
// Иначе проверка «флаги в аргументах» ничего не доказывала бы.
func TestGitProtocolAllowListIsEnforced(t *testing.T) {
	src, _, _ := pinRepo(t, false)

	denyFile := exec.Command("git", append(append([]string{}, gitHarden...),
		"-c", "protocol.file.allow=never",
		"clone", "--depth", "1", "--quiet", "--", src, filepath.Join(t.TempDir(), "x"))...)
	denyFile.Env = gitEnv()
	if out, err := common.CombinedOutput(denyFile); err == nil {
		t.Fatalf("file:// клонирован, хотя протокол не разрешён: %s", out)
	}

	// С настоящим набором тот же клон обязан пройти.
	dst := filepath.Join(t.TempDir(), "y")
	if out, err := runGit("clone", "--depth", "1", "--quiet", "--", src, dst); err != nil {
		t.Fatalf("file:// в рабочем наборе разрешён, но клон упал: %v: %s", err, out)
	}
}

// Реестр по file:// — тот же путь, что у CI на теге (registry-release).
func TestLoadRegistryOverFileURLWithHardenedGit(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "hardened@test")
	git(t, repo, "config", "user.name", "hardened")
	if err := os.WriteFile(filepath.Join(repo, RegistryFile), []byte("version: \"0.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "registry")

	h, err := Load(fileRepoURL(repo))
	if err != nil {
		t.Fatalf("загрузка реестра по file:// с усиленным окружением: %v", err)
	}
	defer h.Close()
	if h.Registry.Version != FormatVersion {
		t.Fatalf("version: %q", h.Registry.Version)
	}
}
