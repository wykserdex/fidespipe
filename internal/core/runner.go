package core

import (
	"github.com/wykserdex/fidespipe/internal/execution"
)

type RunOptions = execution.RunOptions
type RunStats = execution.RunStats

// v0.24: прокидываем opts целиком (ранее — поле-в-поле копирование, которое
// молча теряло новые поля: RunID/GateUI утерлись в первый же прогон API-теста).
func Run(pf *PipelineFile, eng *Engine, opts RunOptions) (RunStats, error) {
	return execution.Run(pf, eng, opts)
}
