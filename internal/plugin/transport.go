package plugin

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

type Transport interface {
	Invoke(manifest *pipeline.Manifest, input []byte) ([]byte, error)
}

type StdioTransport struct {
	Timeout time.Duration
}

func (t *StdioTransport) Invoke(manifest *pipeline.Manifest, input []byte) ([]byte, error) {
	timeout := t.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	res := Exec(manifest, input, timeout)
	if !res.OK() {
		return nil, fmt.Errorf("plugin %s failed: %s (%s)", manifest.ID, res.ErrMsg, res.ErrCode)
	}
	b, err := json.Marshal(res.Output)
	if err != nil {
		return nil, fmt.Errorf("marshal output: %w", err)
	}
	return b, nil
}

const ProtocolVersion = "0.2"

type Envelope struct {
	ProtocolVersion string      `json:"protocol_version"`
	Type            string      `json:"type"`
	RequestID       string      `json:"request_id"`
	Payload         interface{} `json:"payload"`
}

func NewEnvelope(reqID string, payload interface{}) *Envelope {
	return &Envelope{
		ProtocolVersion: ProtocolVersion,
		Type:            "request",
		RequestID:       reqID,
		Payload:         payload,
	}
}

func (e *Envelope) Marshal() ([]byte, error) {
	return json.Marshal(e)
}

func ParseEnvelope(raw []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("envelope parse: %w", err)
	}
	if env.ProtocolVersion != "0.2" && env.ProtocolVersion != "0.3" {
		return nil, fmt.Errorf("unsupported protocol_version %q", env.ProtocolVersion)
	}
	return &env, nil
}
