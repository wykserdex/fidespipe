package bot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config — весь продуктовый слой в одном файле. Секреты живут только здесь,
// сам файл в .gitignore, в репозитории лежит bot.config.example.json.
type Config struct {
	TelegramToken string `json:"telegram_token"`
	CryptoToken   string `json:"cryptobot_token"`
	AdminTGID     int64  `json:"admin_tg_id"`

	// FidespipeBin — бинарь движка; RepoRoot — каталог, из которого видны
	// product/pipelines и plugins/. Бот запускает движок как есть, без правок.
	FidespipeBin string `json:"fidespipe_bin"`
	RepoRoot     string `json:"repo_root"`
	DataDir      string `json:"data_dir"`

	// PythonBin — интерпретатор, в который ставятся доноры (sherlock, holehe).
	// Нужен, чтобы найти каталог её console-скриптов: см. ScriptsDir.
	PythonBin string `json:"python_bin"`

	PriceUSDT           string `json:"price_usdt"`
	FreeCooldownSeconds int    `json:"free_cooldown_seconds"`
	QuickTimeoutSeconds int    `json:"quick_timeout_seconds"`
	FullTimeoutSeconds  int    `json:"full_timeout_seconds"`
	MaxConcurrentRuns   int    `json:"max_concurrent_runs"`
}

func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("конфиг: %w", err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("конфиг %s: %w", path, err)
	}
	if c.TelegramToken == "" {
		return Config{}, fmt.Errorf("конфиг: telegram_token пустой")
	}
	if c.RepoRoot == "" {
		c.RepoRoot = "."
	}
	if c.PythonBin == "" {
		c.PythonBin = "python"
	}
	if c.DataDir == "" {
		c.DataDir = "var/bot"
	}
	if c.PriceUSDT == "" {
		c.PriceUSDT = "3.00"
	}
	if c.FreeCooldownSeconds <= 0 {
		c.FreeCooldownSeconds = 600
	}
	if c.QuickTimeoutSeconds <= 0 {
		c.QuickTimeoutSeconds = 300
	}
	if c.FullTimeoutSeconds <= 0 {
		c.FullTimeoutSeconds = 900
	}
	if c.MaxConcurrentRuns <= 0 {
		c.MaxConcurrentRuns = 2
	}
	// exec.Command разрешает путь к бинарю от cwd бота, а не от cmd.Dir,
	// поэтому абсолютизируем заранее — иначе бот «не находит» движок
	// ровно в тот момент, когда его запустили не из корня репозитория.
	root, err := filepath.Abs(c.RepoRoot)
	if err != nil {
		return Config{}, err
	}
	c.RepoRoot = root
	if c.FidespipeBin == "" {
		return Config{}, fmt.Errorf("конфиг: fidespipe_bin пустой")
	}
	if !filepath.IsAbs(c.FidespipeBin) {
		c.FidespipeBin = filepath.Join(root, c.FidespipeBin)
	}
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(root, c.DataDir)
	}
	return c, nil
}
