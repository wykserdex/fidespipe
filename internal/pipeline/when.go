package pipeline

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wykserdex/fidespipe/internal/runctx"
)

// When — условие выполнения шага (v0.20).
//
// Два формата в YAML:
//
//	when: steps.check.valid                  # строка = путь: шаг, если значение «истинно»
//	when: { path: steps.stats.words, op: gte, value: 10 }
//
// Операторы: truthy (по умолчанию для строки), exists, missing, eq, neq,
// gt, gte, lt, lte, contains.
//
// Смысл «истинности»: bool → само значение; string → не пусто; array/object →
// не пусты; number → не ноль; null → ложь.
type When struct {
	Path  string
	Op    string
	Value interface{}
}

func (w *When) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		if s == "" {
			return fmt.Errorf("when: пустое условие")
		}
		w.Path, w.Op = s, OpTruthy
		return nil
	}
	var m struct {
		Path  string      `yaml:"path"`
		Op    string      `yaml:"op"`
		Value interface{} `yaml:"value"`
	}
	if err := value.Decode(&m); err != nil {
		return err
	}
	if m.Path == "" {
		return fmt.Errorf("when: path обязателен")
	}
	op := m.Op
	if op == "" {
		op = OpEq
	}
	w.Path, w.Op, w.Value = m.Path, op, m.Value
	return nil
}

// IsSet — задано ли условие (пустой When не маршалился).
func (w *When) IsSet() bool { return w.Path != "" }

// String — человекочитаемая форма для лога/плана.
func (w *When) String() string {
	switch w.Op {
	case OpTruthy, OpExists, OpMissing:
		return w.Path + " (" + w.Op + ")"
	default:
		return fmt.Sprintf("%s %s %v", w.Path, w.Op, w.Value)
	}
}

const (
	OpTruthy   = "truthy"
	OpExists   = "exists"
	OpMissing  = "missing"
	OpEq       = "eq"
	OpNeq      = "neq"
	OpGt       = "gt"
	OpGte      = "gte"
	OpLt       = "lt"
	OpLte      = "lte"
	OpContains = "contains"
)

// WhenOps — полный список операторов (для валидатора).
var WhenOps = map[string]bool{
	OpTruthy: true, OpExists: true, OpMissing: true,
	OpEq: true, OpNeq: true, OpGt: true, OpGte: true,
	OpLt: true, OpLte: true, OpContains: true,
}

// EvaluateWhen вычисляет условие против контекста (ctx.Data).
// ok=false — условие ложно (шаг пропускается). error — условие не оценить
// (нет пути при exists-семантике это ok=false, а не ошибка; ошибка — это
// несовместимые типы для сравнения, например gt по строке).
func EvaluateWhen(w When, data map[string]interface{}) (bool, error) {
	v, found := runctx.ResolvePath(w.Path, data)
	switch w.Op {
	case OpExists:
		return found, nil
	case OpMissing:
		return !found, nil
	case OpTruthy:
		if !found {
			return false, nil
		}
		return isTruthy(v), nil
	case OpEq, OpNeq:
		eq, err := valueEqual(v, w.Value, found)
		if err != nil {
			return false, err
		}
		if w.Op == OpEq {
			return eq, nil
		}
		return !eq, nil
	case OpGt, OpGte, OpLt, OpLte:
		lhs, okL := toFloat(v)
		rhs, okR := toFloat(w.Value)
		if !okL || !okR {
			return false, fmt.Errorf("when: %s — числовое сравнение, но значение не число (got %s)", w.Path, typeName(v))
		}
		switch w.Op {
		case OpGt:
			return lhs > rhs, nil
		case OpGte:
			return lhs >= rhs, nil
		case OpLt:
			return lhs < rhs, nil
		default:
			return lhs <= rhs, nil
		}
	case OpContains:
		if !found {
			return false, nil
		}
		return containsValue(v, w.Value), nil
	}
	return false, fmt.Errorf("when: неизвестный оператор %q", w.Op)
}

func isTruthy(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []interface{}:
		return len(x) > 0
	case map[string]interface{}:
		return len(x) > 0
	default:
		f, ok := toFloat(v)
		return ok && f != 0
	}
}

// valueEqual — сравнение «честных» JSON/YAML-значений: числа сравниваются
// независимо от представления (int из YAML = float64 из JSON), null == null
// (нет пути = null).
func valueEqual(a, b interface{}, aFound bool) (bool, error) {
	an, bn := a == nil || !aFound, b == nil
	if an || bn {
		return an && bn, nil
	}
	if af, ok := toFloat(a); ok {
		bf, ok2 := toFloat(b)
		if !ok2 {
			return false, nil
		}
		return af == bf, nil
	}
	if _, ok := toFloat(b); ok {
		return false, nil
	}
	return reflect.DeepEqual(a, b), nil
}

func toFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	default:
		return 0, false
	}
}

func containsValue(haystack, needle interface{}) bool {
	if s, ok := haystack.(string); ok {
		ns, ok := needle.(string)
		if !ok {
			return false
		}
		return strings.Contains(s, ns)
	}
	if arr, ok := haystack.([]interface{}); ok {
		for _, el := range arr {
			eq, err := valueEqual(el, needle, true)
			if err == nil && eq {
				return true
			}
		}
	}
	return false
}

func typeName(v interface{}) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	case bool:
		return "boolean"
	default:
		return "number"
	}
}
