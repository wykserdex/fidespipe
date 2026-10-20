package plugin

// v0.23: контракт рантайма — типы и форматы проверяются, а не только наличие.
// README обещает: «ядро проверяет после каждого запуска». До v0.23
// EnforceOutput принимал {"total": "НЕ ЧИСЛО"} при type: number —
// неправильный тип тихо утекал downstream.

import (
	"fmt"
	"net"
	"regexp"

	"github.com/wykserdex/fidespipe/internal/common"
	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// KindOf — JSON-тип значения (честно для Go-типов тоже).
func KindOf(v interface{}) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case float64, float32, int, int8, int16, int32, int64, uint, uint32, uint64:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	case nil:
		return "null"
	}
	return "unknown"
}

var (
	contractEmailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)
	contractURLRe   = regexp.MustCompile(`^https?://\S+$`)
)

// FormatOK — формат для строк (протокол: text, email, url, ip, file_ref).
// Неизвестный формат — не блокируем (статический валидатор проверяет список).
func FormatOK(format, s string) bool {
	switch format {
	case "", "text":
		return true
	case "email":
		return contractEmailRe.MatchString(s)
	case "url":
		return contractURLRe.MatchString(s)
	case "ip":
		return net.ParseIP(s) != nil
	case "file_ref":
		return s != ""
	}
	return true
}

// CheckValue — контракт для одного значения: тип (если объявлен) +
// формат (для строк). what — «вход X» / «поле Y» для сообщения.
func CheckValue(portName, what string, port pipeline.Port, v interface{}) error {
	if port.Type != "" {
		if k := KindOf(v); k != port.Type {
			return fmt.Errorf("нарушение контракта: %s %s — тип %s, а в манифесте %q (плагин врёт или upstream сломан)",
				what, portName, k, port.Type)
		}
	}
	if port.Format != "" {
		if s, ok := v.(string); ok && !FormatOK(port.Format, s) {
			return fmt.Errorf("нарушение контракта: %s %s не соответствует формату %q: %q",
				what, portName, port.Format, common.Truncate(s, 80))
		}
	}
	return nil
}
