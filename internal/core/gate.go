package core

import (
	"strings"

	"github.com/wykserdex/fidespipe/internal/gate"
)

func runGate(st *Step, ctx *Ctx, j *Journal, opts RunOptions) string {
	svc := gate.NewService()
	return svc.Run(st, ctx, j, gate.GateOptions{Yes: opts.Yes, Quiet: opts.Quiet, Policy: gate.Policy{AllowAutoApprove: !opts.NoAutoApprove}})
}

func kindOf(v interface{}) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case float64:
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

func basename(path string) string {
	parts := strings.Split(path, ".")
	return parts[len(parts)-1]
}
