package core

import (
	"github.com/wykserdex/fidespipe/internal/pipeline"
	"github.com/wykserdex/fidespipe/internal/plugin"
)

type Duration = pipeline.Duration
type PipelineFile = pipeline.PipelineFile
type Pipeline = pipeline.Pipeline
type Step = pipeline.Step
type Retry = pipeline.Retry
type FormField = pipeline.FormField
type Port = pipeline.Port
type Runtime = pipeline.Runtime
type Permissions = pipeline.Permissions
type NetworkPermission = pipeline.NetworkPermission
type Manifest = pipeline.Manifest

const PlatformAPI = pipeline.PlatformAPI

// Engine is plugin.Engine; the old core alias remains for compatibility.
type Engine = plugin.Engine

var NewEngine = plugin.NewEngine
var IsBuiltin = plugin.IsBuiltin

func PortSource(portName string, port Port, st *Step) string {
	return pipeline.PortSource(portName, port, st)
}
