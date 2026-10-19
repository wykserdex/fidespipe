package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeclaredDonorIsReadFromManifest — объявленный в манифесте донор читается
// и не теряется.
//
// Раньше источником правды был только текст описания, и плагин с честным
// рецептом (с версией и хешем) выглядел ровно так же, как плагин с веной в
// прозе: оба требовали «поставь инструмент руками». Объявленный рецепт должен
// доезжать до доктора, иначе механизм установки работает, а диагностика его
// не видит.
func TestDeclaredDonorIsReadFromManifest(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "exiftool")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `id: exiftool
version: 0.1.0
platform_api: "^0.1"
description: 'ExifTool — чтение метаданных.'
author: "test"

runtime:
  type: python
  entry: main.py
  requires: []
  donor:
    kind: binary
    name: exiftool
    version: "13.59"
    url: https://sourceforge.net/projects/exiftool/files/Image-ExifTool-13.59.tar.gz/download
    sha256: 668ea3acececb7235fbd0f4900e72d5f12c9b07e5c778fd36cb1e9b5828fd65a
    target: exiftool
    bin_env: EXIFTOOL_BIN
`
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	manifests := scanManifests(dir)
	if len(manifests) != 1 {
		t.Fatalf("найдено %d манифестов, ожидался 1", len(manifests))
	}
	d := manifests[0].Donor
	if d == nil {
		t.Fatal("объявленный донор не прочитан из манифеста")
	}
	if d.Name != "exiftool" {
		t.Errorf("донор = %q, ожидался exiftool", d.Name)
	}
	if d.Kind != "binary" {
		t.Errorf("вид = %q, ожидался binary", d.Kind)
	}
	if d.SHA256 == "" {
		t.Error("хеш донора не прочитан — без него качать исполняемый код нельзя")
	}
	if d.BinEnv != "EXIFTOOL_BIN" {
		t.Errorf("переменная = %q, ожидалась EXIFTOOL_BIN", d.BinEnv)
	}
}
