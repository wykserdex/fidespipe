package gate

import (
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

func TestAgentAutoAllowedRejectsHumanApproval(t *testing.T) {
	st := &pipeline.Step{
		ID:       "test_gate",
		Approval: "human",
	}
	opts := GateOptions{
		AllowAgentAuto: true,
	}
	if agentAutoAllowed(st, opts) {
		t.Error("agentAutoAllowed должен отклонять approval: human")
	}
}

func TestAgentAutoAllowedPermitsNonHuman(t *testing.T) {
	st := &pipeline.Step{
		ID:       "test_gate",
		Approval: "",
	}
	opts := GateOptions{
		AllowAgentAuto: true,
	}
	if !agentAutoAllowed(st, opts) {
		t.Error("agentAutoAllowed должен разрешать не-human гейты")
	}
}

func TestAgentAutoAllowedRejectsWhenFlagDisabled(t *testing.T) {
	st := &pipeline.Step{
		ID:       "test_gate",
		Approval: "",
	}
	opts := GateOptions{
		AllowAgentAuto: false,
	}
	if agentAutoAllowed(st, opts) {
		t.Error("agentAutoAllowed должен отклонять когда AllowAgentAuto=false")
	}
}

func TestSourceAgentAutoConstant(t *testing.T) {
	if SourceAgentAuto != "agent_auto" {
		t.Error("SourceAgentAuto должен быть 'agent_auto'")
	}
}
