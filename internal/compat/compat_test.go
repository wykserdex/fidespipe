package compat

import (
	"os"
	"testing"
)

// TestInitCarriesLegacyValues — прежнее имя переменной окружения работает.
//
// Проверяется не «переменная вообще есть», а ровно то, ради чего совместимость
// и нужна: человек поставил WEDRA_TRUST_CONFIG в CI или в скрипте, а код
// переименовали. Настройка должна продолжать работать, а не исчезнуть молча.
func TestInitCarriesLegacyValues(t *testing.T) {
	t.Setenv("FIDESPIPE_TRUST_CONFIG", "")
	t.Setenv("WEDRA_TRUST_CONFIG", "/старый/путь/fidespipe-trust.yaml")
	Init()
	if got := envValue("FIDESPIPE_TRUST_CONFIG"); got != "/старый/путь/fidespipe-trust.yaml" {
		t.Fatalf("прежнее значение не перенесено: %q", got)
	}
}

// TestInitPrefersNewValue — явно заданная новая переменная побеждает.
//
// Иначе старая, оставшаяся в чьём-то окружении, тихо перебивала бы новую
// настройку.
func TestInitPrefersNewValue(t *testing.T) {
	t.Setenv("FIDESPIPE_TRUST_CONFIG", "/новый/путь")
	t.Setenv("WEDRA_TRUST_CONFIG", "/старый/путь")
	Init()
	if got := envValue("FIDESPIPE_TRUST_CONFIG"); got != "/новый/путь" {
		t.Fatalf("новая переменная должна побеждать, получили %q", got)
	}
}

// TestInitIsIdempotent — повторный вызов ничего не портит.
func TestInitIsIdempotent(t *testing.T) {
	t.Setenv("FIDESPIPE_DONOR_ROOT", "")
	t.Setenv("WEDRA_DONOR_ROOT", "/старый/доноры")
	Init()
	Init()
	if got := envValue("FIDESPIPE_DONOR_ROOT"); got != "/старый/доноры" {
		t.Fatalf("повторный вызов изменил значение: %q", got)
	}
}

// envValue — чтение переменной окружения.
func envValue(name string) string { return os.Getenv(name) }
