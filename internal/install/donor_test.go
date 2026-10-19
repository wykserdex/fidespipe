package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/pipeline"
)

// tlsDonorServer — сервер для тестов установки донора.
//
// Именно TLS: правило «только https» в реализации абсолютное, и ослаблять его
// ради теста нельзя. Клиент подменяется на доверяющий сертификату сервера.
func tlsDonorServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	old := donorHTTPClient
	donorHTTPClient = srv.Client()
	t.Cleanup(func() { donorHTTPClient = old })
	return srv
}

// sha256Of — хеш содержимого, для подстановки в рецепт.
func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestInstallDonorScript — вид script: файл качается, хеш сверяется, шебанг
// дописывается, файл становится исполняемым.
func TestInstallDonorScript(t *testing.T) {
	body := []byte("print('hi')\n")
	srv := tlsDonorServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	defer srv.Close()

	dir := t.TempDir()
	d := &pipeline.Donor{
		Kind: "script", Name: "tool", URL: srv.URL + "/tool.sh",
		SHA256: sha256Of(body), Interpreter: "perl", BinEnv: "TOOL_BIN",
	}
	path, err := installDonor(d, dir)
	if err != nil {
		t.Fatalf("установка донора: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "#!/usr/bin/env perl") {
		t.Errorf("шебанг не дописан: %q", string(raw[:min(40, len(raw))]))
	}
	// Бит исполнения проверяется только на Unix: на Windows права задаются
	// через ACL, и режим файла в chmod-смысле meaningless — там проверять
	// нечего.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("файл донора не исполняемый: %v", fi.Mode().Perm())
		}
	}
}

// TestInstallDonorRejectsWrongHash — ГЛАВНАЯ проверка: битый хеш обязан
// отвергаться. Это ровно та защита, ради которой хеш и объявлен.
func TestInstallDonorRejectsWrongHash(t *testing.T) {
	body := []byte("payload")
	srv := tlsDonorServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	defer srv.Close()

	dir := t.TempDir()
	d := &pipeline.Donor{
		Kind: "script", Name: "tool", URL: srv.URL + "/tool.sh",
		SHA256: sha256Of([]byte("другое")), Interpreter: "perl",
	}
	if _, err := installDonor(d, dir); err == nil {
		t.Fatal("битый хеш обязан давать ошибку")
	} else if !strings.Contains(err.Error(), "хеш") {
		t.Errorf("в ошибке нет упоминания хеша: %v", err)
	}
}

// TestInstallDonorRequiresHash — без хеша качать нельзя: исполняемый код из
// сети без сверки это не установка, а закладка.
func TestInstallDonorRequiresHash(t *testing.T) {
	srv := tlsDonorServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	})
	defer srv.Close()
	dir := t.TempDir()
	d := &pipeline.Donor{Kind: "script", Name: "tool", URL: srv.URL + "/t.sh", Interpreter: "sh"}
	if _, err := installDonor(d, dir); err == nil {
		t.Fatal("без sha256 установка обязана отвергаться")
	}
}

// TestInstallDonorRejectsHTTP — только https. http — это не защита.
func TestInstallDonorRejectsHTTP(t *testing.T) {
	dir := t.TempDir()
	d := &pipeline.Donor{
		Kind: "script", Name: "tool", URL: "http://example.com/t.sh",
		SHA256: strings.Repeat("a", 64), Interpreter: "sh",
	}
	if _, err := installDonor(d, dir); err == nil {
		t.Fatal("http-адрес обязан отвергаться")
	}
}

// TestInstallDonorBinaryFromTarGz — вид binary: архив распаковывается, нужный
// файл выбирается по имени, права выставляются.
func TestInstallDonorBinaryFromTarGz(t *testing.T) {
	content := []byte("#!/bin/sh\necho tool\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "tool-1.0/bin/tool", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	archive := buf.Bytes()

	srv := tlsDonorServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	defer srv.Close()

	dir := t.TempDir()
	d := &pipeline.Donor{
		Kind: "binary", Name: "tool", URL: srv.URL + "/tool.tar.gz",
		SHA256: sha256Of(archive), Target: "tool",
	}
	path, err := installDonor(d, dir)
	if err != nil {
		t.Fatalf("установка бинаря: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, content) {
		t.Errorf("содержимое бинаря не совпало: %q", string(raw))
	}
}

// TestExtractDonorRejectsPathTraversal — архив из сети не должен уметь писать
// мимо каталога установки.
func TestExtractDonorRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "escaped.txt")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"../escaped.txt", "/abs/escaped.txt", "sub/ok.txt"} {
		content := []byte("x")
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()

	archive := filepath.Join(dir, "a.tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := extractDonor(archive, "https://example.com/a.tar.gz", dir)
	if err != nil {
		t.Fatalf("распаковка: %v", err)
	}
	for _, f := range files {
		if !strings.HasPrefix(f, filepath.Clean(dir)+string(os.PathSeparator)) {
			t.Errorf("файл извлечён наружу: %s", f)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("файл записан за пределы каталога установки")
	}
}

// TestInstallDonorRejectsUnknownKind — неизвестный вид донора отвергается, а не
// трактуется как «установить как-нибудь».
func TestInstallDonorRejectsUnknownKind(t *testing.T) {
	dir := t.TempDir()
	d := &pipeline.Donor{Kind: "магия", Name: "tool"}
	if _, err := installDonor(d, dir); err == nil {
		t.Fatal("неизвестный вид донора обязан давать ошибку")
	}
}

// TestDonorDirIsOutsidePluginDir — донор не кладётся в каталог плагина: доверие
// выдаётся по хэшу содержимого каталога, и установка донора внутрь сняла бы
// доверие ровно в момент установки.
func TestDonorDirIsOutsidePluginDir(t *testing.T) {
	dir := donorDirFor("plugins", "exiftool")
	if strings.HasPrefix(filepath.Clean(dir), filepath.Clean("plugins")+string(os.PathSeparator)+"exiftool") {
		t.Errorf("каталог донора внутри каталога плагина: %s", dir)
	}
	if !strings.Contains(dir, ".donors") {
		t.Errorf("каталог донора не спрятан: %s", dir)
	}
}

// TestInstallDonorRawBinary — скачивание без архива: файл сам и есть донор.
//
// Отдельный случай, потому что имя во временном файле имеет префикс
// «.donor-», и поиск по имени внутри «архива» никогда бы не нашёл сам файл.
func TestInstallDonorRawBinary(t *testing.T) {
	content := []byte("#!/bin/sh\necho tool\n")
	srv := tlsDonorServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	})
	defer srv.Close()

	dir := t.TempDir()
	d := &pipeline.Donor{
		Kind: "binary", Name: "tool", URL: srv.URL + "/tool",
		SHA256: sha256Of(content),
	}
	path, err := installDonor(d, dir)
	if err != nil {
		t.Fatalf("установка бинаря: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, content) {
		t.Errorf("содержимое не совпало: %q", string(raw))
	}
	if filepath.Base(path) != "tool" {
		t.Errorf("бинарь назван %q, ожидался tool", filepath.Base(path))
	}
}
