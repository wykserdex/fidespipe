package install

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// Максимальный размер донора. Ограничение не паранойя, а защита от
// бесконечной загрузки: без него сервер может отдавать поток, и установка
// плагина будет есть диск до конца.
const maxDonorBytes = 256 << 20

// donorTimeout — потолок на скачивание донора.
const donorTimeout = 5 * time.Minute

// donorInstall — точка подмены для тестов, как pipInstall.
var donorInstall = installDonor

// donorHTTPClient — клиент загрузки донора. Отдельная переменная, а не
// создание внутри функции: тесту нужен клиент, доверяющий его собственному
// сертификату, и правило «только https» ослаблять ради этого нельзя.
var donorHTTPClient = &http.Client{Timeout: donorTimeout}

// donorDirFor — каталог, куда кладётся донор плагина.
//
// ВНЕ каталога плагина, и это не выбор вкуса: доверие выдаётся по хэшу
// содержимого каталога, и установка донора внутрь изменила бы хэш — плагин
// перестал бы быть доверенным ровно в момент установки.
//
// Каталог начинается с точки, чтобы не мешать перебору плагинов.
func donorDirFor(destRoot, pluginName string) string {
	return filepath.Join(destRoot, ".donors", pluginName)
}

// installDonor — ставит донора по рецепту и возвращает путь к исполняемому файлу.
//
// Три вида, и у каждого своя проверка:
//
//	go     — ставится самим Go: `go install module@version`. Контрольные суммы
//	         проверяет прокси Go, и подменять донор после установки нечем.
//	binary — качается готовый файл: СНАЧАЛА сверяется sha256, и только потом
//	         распаковывается. Качать исполняемый код и запускать, не проверив
//	         хеш, — это не установка, а закладка.
//	script — то же, плюс строка-шебанг, если её нет в файле.
func installDonor(d *pipeline.Donor, dir string) (string, error) {
	if d == nil {
		return "", fmt.Errorf("рецепт донора не задан")
	}
	if strings.TrimSpace(d.Name) == "" {
		return "", fmt.Errorf("рецепт донора без имени")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("каталог донора: %w", err)
	}
	switch d.Kind {
	case "go":
		return installGoDonor(d, dir)
	case "binary":
		return installBinaryDonor(d, dir)
	case "script":
		return installScriptDonor(d, dir)
	default:
		return "", fmt.Errorf("вид донора %q неизвестен: допускаются go, binary, script", d.Kind)
	}
}

// installGoDonor — go install в заданный каталог.
//
// GOBIN направлен в каталог донора, поэтому бинарь кладётся рядом с плагином,
// а не в ~/go/bin: иначе донор был бы в одном месте при установке, а плагин
// искал бы его в другом.
func installGoDonor(d *pipeline.Donor, dir string) (string, error) {
	if strings.TrimSpace(d.Module) == "" {
		return "", fmt.Errorf("вид донора go: не указан module")
	}
	ver := strings.TrimSpace(d.Version)
	if ver == "" {
		ver = "latest"
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("вид донора go: нужен инструмент Go, а его нет: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), donorTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "install", d.Module+"@"+ver)
	cmd.Env = append(os.Environ(), "GOBIN="+dir, "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go install %s@%s: %w: %s", d.Module, ver, err, tail(string(out), 400))
	}
	path := filepath.Join(dir, d.Name)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("go install %s@%s отработал, но %s не появился", d.Module, ver, path)
	}
	return path, nil
}

// installBinaryDonor — скачать готовый бинарь, сверить хеш, распаковать.
func installBinaryDonor(d *pipeline.Donor, dir string) (string, error) {
	if strings.TrimSpace(d.URL) == "" {
		return "", fmt.Errorf("вид донора binary: не указан url")
	}
	archivePath, err := downloadDonor(d.URL, dir, d.SHA256)
	if err != nil {
		return "", err
	}
	defer os.Remove(archivePath)

	target := strings.TrimSpace(d.Target)
	if target == "" {
		target = d.Name
	}
	// Не архив — значит скачанный файл и есть донор. Отдельная ветка нужна
	// потому, что во временном имени файла есть префикс «.donor-», и поиск по
	// имени в «архиве» никогда бы не нашёл сам файл.
	if !isArchiveURL(d.URL) {
		final := filepath.Join(dir, d.Name)
		if err := moveFile(archivePath, final); err != nil {
			return "", err
		}
		if err := os.Chmod(final, 0o755); err != nil {
			return "", fmt.Errorf("права на донора: %w", err)
		}
		return final, nil
	}
	extracted, err := extractDonor(archivePath, d.URL, dir)
	if err != nil {
		return "", err
	}
	// Поиск по имени файла, а совпадений должно быть РОВНО одно: два файла с
	// одним именем в разных каталогах архива означают, что рецепт неоднозначен,
	// и гадать нельзя.
	var founds []string
	for _, cand := range extracted {
		if filepath.Base(cand) == target {
			founds = append(founds, cand)
		}
	}
	switch len(founds) {
	case 0:
		return "", fmt.Errorf("в архиве %s нет файла %q", filepath.Base(archivePath), target)
	case 1:
	default:
		return "", fmt.Errorf("в архиве %s файл %q встречается %d раз — рецепт неоднозначен",
			filepath.Base(archivePath), target, len(founds))
	}
	// Файл ОСТАЁТСЯ ТАМ, ГДЕ ЛЕЖИТ В АРХИВЕ. Это не деталь: exiftool ищет
	// свою библиотеку относительно себя («$1/lib»), joomscan — модули рядом с
	// собой. Если вытащить один файл и выбросить остальное, донор будет
	// нерабочим, и выглядеть это будет как «установилось».
	final := founds[0]
	if err := os.Chmod(final, 0o755); err != nil {
		return "", fmt.Errorf("права на донора: %w", err)
	}
	return final, nil
}

// installScriptDonor — скачать скрипт, сверить хеш, дописать шебанг.
func installScriptDonor(d *pipeline.Donor, dir string) (string, error) {
	if strings.TrimSpace(d.URL) == "" {
		return "", fmt.Errorf("вид донора script: не указан url")
	}
	tmp, err := downloadDonor(d.URL, dir, d.SHA256)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	raw, err := os.ReadFile(tmp)
	if err != nil {
		return "", err
	}
	body := string(raw)
	if !strings.HasPrefix(body, "#!") {
		interp := strings.TrimSpace(d.Interpreter)
		if interp == "" {
			return "", fmt.Errorf("вид донора script: нет интерпретатора и в файле нет шебанга")
		}
		body = "#!/usr/bin/env " + interp + "\n" + body
	}
	final := filepath.Join(dir, d.Name)
	if err := os.WriteFile(final, []byte(body), 0o755); err != nil {
		return "", fmt.Errorf("запись донора: %w", err)
	}
	// Права выставляются явно: umask может забрать бит исполнения, и тогда
	// скрипт скачался, но не запускается — с сообщением «permission denied».
	if err := os.Chmod(final, 0o755); err != nil {
		return "", fmt.Errorf("права на донора: %w", err)
	}
	return final, nil
}

// downloadDonor — скачать файл и сверить sha256.
//
// Хеш сверяется ДО того, как файл будет распакован или запущен. Порядок
// наоборот — обычная, но фатальная ошибка: распаковать и сначала поверить.
func downloadDonor(url, dir, wantSHA string) (string, error) {
	if !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("адрес донора %q: только https, http — это не защита", url)
	}
	resp, err := donorHTTPClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("загрузка донора: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("загрузка донора: %s ответил %s", url, resp.Status)
	}
	name := filepath.Base(url)
	if name == "" || name == "." || name == "/" {
		name = "donor.bin"
	}
	tmp := filepath.Join(dir, ".donor-"+name)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("файл донора: %w", err)
	}
	// Временный файл НЕ удаляется здесь: загрузка только что его создала, а
	// читать его будет вызывающий. Уборка — его дело (у обоих вызывающих уже
	// есть свой defer os.Remove).

	limited := io.LimitReader(resp.Body, maxDonorBytes+1)
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), limited)
	if err != nil {
		f.Close()
		return "", fmt.Errorf("загрузка донора: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("файл донора: %w", err)
	}
	if n > maxDonorBytes {
		return "", fmt.Errorf("донор больше %d байт", maxDonorBytes)
	}
	want := strings.ToLower(strings.TrimSpace(wantSHA))
	if want == "" {
		return "", fmt.Errorf("в рецепте донора не указан sha256 — качать исполняемый код без сверки нельзя")
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != want {
		return "", fmt.Errorf("хеш донора не сошёлся: ожидался %s, получен %s", want, got)
	}
	return tmp, nil
}

// isArchiveURL — архив ли это по расширению адреса.
//
// Расширение, а не сигнатура файла: сигнатуру можно и подделать, а вот что
// именно скачает оператор, решает расширение в рецепте, и его проверяет
// человек, читающий манифест.
func isArchiveURL(url string) bool {
	base := strings.ToLower(url)
	return strings.HasSuffix(base, ".zip") ||
		strings.HasSuffix(base, ".tar.gz") ||
		strings.HasSuffix(base, ".tgz") ||
		strings.HasSuffix(base, ".tar")
}

// extractDonor — распаковать архив, защитившись от выхода за пределы каталога.
//
// Записи с «..» или абсолютным путём пропускаются: архив из сети не должен
// уметь писать мимо каталога установки. Симлинки тоже пропускаются — иначе
// распаковка создала бы ссылку на что угодно.
func extractDonor(archivePath, url, dir string) ([]string, error) {
	base := strings.ToLower(url)
	switch {
	case strings.HasSuffix(base, ".zip"):
		return extractZip(archivePath, dir)
	case strings.HasSuffix(base, ".tar.gz"), strings.HasSuffix(base, ".tgz"):
		return extractTarGz(archivePath, dir)
	case strings.HasSuffix(base, ".tar"):
		return extractTar(archivePath, dir)
	default:
		// Не архив: считаем самим бинарником.
		return []string{archivePath}, nil
	}
}

func extractTarGz(archivePath, dir string) ([]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("распаковка донора: %w", err)
	}
	defer gz.Close()
	return extractTarStream(gz, dir)
}

func extractTar(archivePath, dir string) ([]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return extractTarStream(f, dir)
}

func extractTarStream(r io.Reader, dir string) ([]string, error) {
	tr := tar.NewReader(r)
	var out []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("распаковка донора: %w", err)
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			continue
		}
		if !hdr.FileInfo().Mode().IsRegular() {
			continue
		}
		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			continue
		}
		dst := filepath.Join(dir, name)
		if !strings.HasPrefix(dst, filepath.Clean(dir)+string(os.PathSeparator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
		out = append(out, dst)
	}
	return out, nil
}

func extractZip(archivePath, dir string) ([]string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("распаковка донора: %w", err)
	}
	defer zr.Close()
	var out []string
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() || zf.Mode()&os.ModeSymlink != 0 {
			continue
		}
		name := filepath.Clean(zf.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			continue
		}
		dst := filepath.Join(dir, name)
		if !strings.HasPrefix(dst, filepath.Clean(dir)+string(os.PathSeparator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			rc.Close()
			return nil, err
		}
		_, cerr := io.Copy(f, rc)
		rc.Close()
		f.Close()
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, dst)
	}
	return out, nil
}

// moveFile — перенести файл, заменив destination.
func moveFile(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		// Файловая система может не дать переименование через границу
		// (разные тома на Windows) — тогда копируем.
		raw, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, raw, 0o755); err != nil {
			return err
		}
		return os.Remove(src)
	}
	return nil
}

// tail — последние n символов вывода команды.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
