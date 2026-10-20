package core

import (
	"time"

	"github.com/wykserdex/fidespipe/internal/common"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

type ExecResult struct {
	ExitCode  int
	Status    string
	Output    map[string]interface{}
	ErrCode   string
	ErrMsg    string
	Retryable bool
	Platform  bool
	TimedOut  bool
	Stderr    string
	Duration  time.Duration
}

func (r *ExecResult) OK() bool {
	return !r.Platform && r.ExitCode == 0 && r.Status == "ok"
}

func (r *ExecResult) shouldRetry() bool {
	if r.TimedOut {
		return true
	}
	return r.Status == "error" && !r.Platform && r.Retryable
}

func fromPluginRes(pr *plugin.ExecResult) *ExecResult {
	res := &ExecResult{
		ExitCode:  pr.ExitCode,
		Output:    pr.Output,
		ErrCode:   pr.ErrCode,
		ErrMsg:    pr.ErrMsg,
		Retryable: pr.Retryable,
		Platform:  pr.Platform,
		Stderr:    pr.Stderr,
		Duration:  pr.Duration,
	}
	if pr.OK() {
		res.Status = "ok"
	} else {
		res.Status = "error"
	}
	if pr.ErrCode == "timeout" {
		res.TimedOut = true
	}
	return res
}

// execPlugin / execPluginEnv — тонкие обёртки над запуском плагина.
//
// Ими пользуются только тесты: раннер вызывает plugin.ExecWithEnvCtx напрямую,
// потому что у него есть ctx с политикой доверия. Здесь ctx нет, и после
// инверсии доверия (H1) это означало бы «никто не доверен» — то есть отказ по
// политике вместо проверки поведения. Поэтому доверие фикстурам выдаётся явно
// и по хэшу содержимого (см. helpers_test.go).
// Запуск плагина в раннере идёт напрямую через plugin.ExecWithEnvCtx: у
// раннера есть ctx с политикой доверия, и подменять его контекстом без
// политики нельзя (после инверсии доверия H1 такой вызов означал бы «никто не
// доверен»). Обёртки execPlugin/execPluginEnv живут в helpers_test.go — ими
// пользуются только тесты, и там доверие фикстурам выдаётся явно.

func EnforceOutput(m *Manifest, out map[string]interface{}) (map[string]interface{}, []string, error) {
	return plugin.EnforceOutput(m, out)
}

func truncate(s string, n int) string {
	return common.Truncate(s, n)
}

func buildInput(m *Manifest, st *Step, ctx *Ctx) (map[string]interface{}, error) {
	in := map[string]interface{}{}
	for name, port := range m.Input {
		from := PortSource(name, port, st)
		v, ok := ctx.Get(from)
		if !ok {
			// Optional без объявленного bind — законно: порт не передан, плагин
			// берёт значение по умолчанию. Но если bind объявлен и не резолвится,
			// это ошибка пайплайна: раньше такой optional-порт молча пропускался,
			// и ран завершался "done", хотя YAML задавал другое значение.
			if st != nil {
				if _, declared := st.Bind[name]; declared {
					return nil, &errorString{msg: "вход " + name + ": bind " + from +
						" не найден в контексте (проверьте ссылку input.*/steps.*)"}
				}
			}
			if port.Optional {
				continue
			}
			return nil, &errorString{msg: "вход " + name + ": путь " + from + " не найден"}
		}
		in[name] = v
	}
	return in, nil
}

type errorString struct{ msg string }

func (e *errorString) Error() string { return e.msg }
