package registry

// Вызовы git из этого пакета идут с пользовательским окружением: конфиг
// машины, credential-helper, insteadOf и protocol.* из ~/.gitconfig влияли на
// то, что клонируется, и на то, откуда. Реестр — это исполняемый ввод (из него
// ставятся плагины), поэтому «что именно ляжет на диск» обязано задаваться
// здесь, а не наследоваться от того, кто запустил fidespipe.
//
// Три меры, все дешёвые и все fail-closed:
//
//   - GIT_CONFIG_GLOBAL указывает на нулевое устройство, то есть глобальный
//     конфиг не читается ВООБЩЕ. os.DevNull, а не константа "/dev/null":
//     на Windows это "NUL", и строкой "/dev/null" git.exe такой путь не
//     понимает. Побочный эффект — пропадает и http.sslCAInfo из ~/.gitconfig,
//     то есть в корпоративной сети с системным CA клон https перестанет
//     проходить. Это осознанный размен: fail-closed вместо тихой подмены
//     источника. Системный конфиг (/etc/gitconfig) при этом остаётся.
//   - GIT_TERMINAL_PROMPT=0 — git не спросит пароль интерактивно и не зависнет
//     на tty, которого у агента всё равно нет.
//   - protocol.allow=never плюс явный список — git не поднимет ни один
//     протокол, которого не назвали явно. Без этого `ext::` выполняет
//     произвольную команду (CVE-2022-39253), а http:// и git:// едут без
//     шифрования и без проверки сервера.
//
// Почему file в списке. `source` в реестре может быть локальным путём или
// file://-URL, и это проверяется ValidateSource, а не этим списком. Запретив
// file, мы сломали бы установку из локального репозитория и существующие
// тесты (TestLoadClonesURLSourceAfterValidation, TestPinCloneOK клонируют
// file://). file не даёт исполнения: это чтение с локального диска, путь к
// которому и так выбирает оператор.

import (
	"os"
	"os/exec"

	"github.com/wykserdex/fidespipe/internal/common"
)

// gitHarden — префикс аргументов: протоколы, которых не назвали, запрещены.
var gitHarden = []string{
	"-c", "protocol.allow=never",
	"-c", "protocol.https.allow=always",
	"-c", "protocol.ssh.allow=always",
	"-c", "protocol.file.allow=always",
}

// gitEnv — окружение, в котором git не доверяет машине.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)
}

// gitCmd — единственная точка сборки команды git в пакете. Всё остальное
// зовёт её, чтобы правило не разъезжалось по коду (та же причина, по
// которой ProcWaitDelay живёт в internal/common, а не на каждом сайте).
func gitCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("git", append(append([]string{}, gitHarden...), args...)...)
	cmd.Env = gitEnv()
	return cmd
}

// runGit — запуск с нашим ProcWaitDelay, вывод и ошибка наружу.
func runGit(args ...string) ([]byte, error) {
	return common.CombinedOutput(gitCmd(args...))
}
