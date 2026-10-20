package core

import ctxpkg "github.com/wykserdex/fidespipe/internal/runctx"

type Ctx = ctxpkg.Ctx

func NewCtx(input map[string]interface{}) *Ctx {
	return ctxpkg.NewCtx(input)
}
