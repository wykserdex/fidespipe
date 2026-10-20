package plugin

// v0.23: контракт-тесты — типы и форматы проверяются после каждого запуска.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

func TestKindOf(t *testing.T) {
	cases := []struct {
		v    interface{}
		want string
	}{
		{true, "boolean"},
		{float64(42), "number"},
		{int(7), "number"},
		{"x", "string"},
		{[]interface{}{1}, "array"},
		{map[string]interface{}{"a": 1}, "object"},
		{nil, "null"},
		{struct{ X int }{1}, "unknown"},
	}
	for _, c := range cases {
		if got := KindOf(c.v); got != c.want {
			t.Errorf("KindOf(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestFormatOK(t *testing.T) {
	cases := []struct {
		format, s string
		want      bool
	}{
		{"email", "a@b.c", true},
		{"email", "not-an-email", false},
		{"url", "https://example.com/x", true},
		{"url", "ftp://nope", false},
		{"url", "example.com", false},
		{"ip", "10.0.0.1", true},
		{"ip", "999.1.1.1", false},
		{"file_ref", "/tmp/x", true},
		{"file_ref", "", false},
		{"text", "любой текст", true},
		{"", "x", true},
		{"что-то_неизвестное", "x", true},
	}
	for _, c := range cases {
		if got := FormatOK(c.format, c.s); got != c.want {
			t.Errorf("FormatOK(%q, %q) = %v, want %v", c.format, c.s, got, c.want)
		}
	}
}

func TestEnforceOutputTypeViolation(t *testing.T) {
	// Эксперимент из ревью v0.23: {"total": "НЕ ЧИСЛО"} при type: number
	m := &pipeline.Manifest{
		ID:     "liar",
		Output: map[string]pipeline.Port{"total": {Type: "number"}},
	}
	_, _, err := EnforceOutput(m, map[string]interface{}{"total": "НЕ ЧИСЛО"})
	if err == nil {
		t.Fatal("ожидалось нарушение контракта (string вместо number)")
	}
	if !strings.Contains(err.Error(), "нарушение контракта") {
		t.Fatalf("сообщение не про контракт: %v", err)
	}
}

func TestEnforceOutputFormatViolation(t *testing.T) {
	m := &pipeline.Manifest{
		ID:     "mail",
		Output: map[string]pipeline.Port{"email": {Type: "string", Format: "email"}},
	}
	_, _, err := EnforceOutput(m, map[string]interface{}{"email": "не почта"})
	if err == nil {
		t.Fatal("ожидалось нарушение формата email")
	}
	if !strings.Contains(err.Error(), "формату") {
		t.Fatalf("сообщение не про формат: %v", err)
	}
}

func TestEnforceOutputOK(t *testing.T) {
	m := &pipeline.Manifest{
		ID: "ok",
		Output: map[string]pipeline.Port{
			"total": {Type: "number"},
			"mail":  {Type: "string", Format: "email"},
			"note":  {Type: "string", Optional: true},
		},
	}
	clean, dropped, err := EnforceOutput(m, map[string]interface{}{
		"total": float64(42), "mail": "a@b.c", "лишний": 1,
	})
	if err != nil {
		t.Fatalf("ожидалось ok: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "лишний" {
		t.Fatalf("лишнее должно отбрасываться: %v", dropped)
	}
	if clean["total"] == nil || clean["note"] != nil {
		t.Fatalf("clean некорректен: %v", clean)
	}
}

func TestEnforceOutputRequiredMissing(t *testing.T) {
	m := &pipeline.Manifest{
		ID:     "req",
		Output: map[string]pipeline.Port{"total": {Type: "number"}},
	}
	_, _, err := EnforceOutput(m, map[string]interface{}{})
	if err == nil {
		t.Fatal("ожидалась ошибка обязательного поля")
	}
}

func TestValidateManifestRejectsInvalidContract(t *testing.T) {
	base := func() *pipeline.Manifest {
		return &pipeline.Manifest{
			ID:          "safe_plugin",
			Version:     "0.1.0",
			PlatformAPI: "^0.1",
			Runtime:     pipeline.Runtime{Type: "python", Entry: "main.py"},
			Output:      map[string]pipeline.Port{"result": {Type: "string"}},
		}
	}
	cases := []struct {
		name string
		edit func(*pipeline.Manifest)
	}{
		{"version", func(m *pipeline.Manifest) { m.Version = "latest" }},
		{"platform", func(m *pipeline.Manifest) { m.PlatformAPI = "^9.0" }},
		{"entry", func(m *pipeline.Manifest) { m.Runtime.Entry = "../outside.py" }},
		{"port_type", func(m *pipeline.Manifest) { m.Output["result"] = pipeline.Port{Type: "unknown"} }},
		{"port_format", func(m *pipeline.Manifest) { m.Output["result"] = pipeline.Port{Type: "array", Format: "text"} }},
		{"secret", func(m *pipeline.Manifest) { m.Permissions.Secrets = []string{"BAD-KEY"} }},
		{"network", func(m *pipeline.Manifest) {
			m.Permissions.Network = []pipeline.NetworkPermission{{Host: "https://example.com", Port: 443}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.edit(m)
			if err := pipeline.ValidateManifest(m); err == nil {
				t.Fatal("ожидалась ошибка манифеста")
			}
		})
	}
}

func TestEnvelopeUsesCanonicalProtocolVersion(t *testing.T) {
	env := NewEnvelope("req", map[string]string{"ok": "yes"})
	if env.ProtocolVersion != ProtocolVersion {
		t.Fatalf("unexpected envelope protocol version: %q", env.ProtocolVersion)
	}
}

func TestManifestRequirementsRequireExactLock(t *testing.T) {
	m := &pipeline.Manifest{
		ID:          "locked_plugin",
		Version:     "0.1.0",
		PlatformAPI: "^0.1",
		Runtime:     pipeline.Runtime{Type: "python", Entry: "main.py", Requires: []string{"requests==2.32.3"}},
		Output:      map[string]pipeline.Port{"result": {Type: "string"}},
	}
	if err := pipeline.ValidateManifest(m); err != nil {
		t.Fatalf("requirement syntax should be valid without a directory: %v", err)
	}
	m.Dir = t.TempDir()
	if err := pipeline.ValidateManifest(m); err == nil {
		t.Fatal("missing requirements.lock was accepted")
	}
	if err := os.WriteFile(filepath.Join(m.Dir, "requirements.lock"), []byte("requests==2.32.3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.ValidateManifest(m); err != nil {
		t.Fatalf("valid requirements.lock rejected: %v", err)
	}
}

func TestDecodeManifestRejectsUnknownFields(t *testing.T) {
	raw := []byte("id: safe_plugin\nversion: 0.1.0\nplatform_api: ^0.1\nruntime:\n  type: python\n  entry: main.py\noutput:\n  result:\n    type: string\nunknown: true\n")
	var m pipeline.Manifest
	if err := pipeline.DecodeManifest(raw, &m); err == nil {
		t.Fatal("неизвестное поле манифеста должно отклоняться")
	}
}
