package core

import "github.com/wykserdex/fidespipe/internal/pipeline"

func LoadPipelineFile(path string) (*PipelineFile, error) {
	return pipeline.LoadPipelineFile(path)
}
