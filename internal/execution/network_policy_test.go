package execution

import (
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// netEngine отдаёт фиксированный манифест с заданным сетевым объявлением.
type netEngine struct {
	net []pipeline.NetworkPermission
}

func (e netEngine) LoadManifest(ref string) (*pipeline.Manifest, error) {
	return &pipeline.Manifest{ID: ref, Permissions: pipeline.Permissions{Network: e.net}}, nil
}

func netPipeline(network string) *pipeline.PipelineFile {
	return &pipeline.PipelineFile{
		FormatVersion: "0.2",
		Pipeline: pipeline.Pipeline{
			Name:    "net",
			Network: network,
			Input:   map[string]interface{}{},
			Steps: []pipeline.Step{
				{ID: "s", Plugin: "acme/probe"},
			},
		},
	}
}

var structuredNet = []pipeline.NetworkPermission{{Host: "api.example.com", Port: 443}}

var blanketNet = []pipeline.NetworkPermission{{AnyHost: true}}

// The network gate. deny is the default, "allow" is an explicit opt-in, and
// only an any_host declaration is actually enforceable.
//
// An omitted `network` field is deny, exactly like an explicit `network: deny`.
// It used to be neither: the gate compared `p.Network == "deny"` and
// `== "allow"`, so an empty field matched no branch, ran no check at all, and
// the subprocess was handed FIDESPIPE_NETWORK=allow — fail-open while the docs
// promised deny. The unset rows below are the regression for that.
//
// Assertions use ASCII markers on purpose: the messages this gate produces are
// Russian, and a test matching on Russian literals was silently corrupted by an
// editor rewriting the file in another encoding, reddening a passing suite for
// the wrong reason. The unset rows match on "не задано", which is present in
// both the refusal and the fix hint and does not depend on the full wording.
func TestNetworkPolicyMatrix(t *testing.T) {
	cases := []struct {
		name     string
		network  string
		net      []pipeline.NetworkPermission
		wantDeny bool
		marker   string
	}{
		{"deny, host:port declared", "deny", structuredNet, true, "network: deny"},
		{"allow, host:port declared", "allow", structuredNet, true, "host:port"},
		{"allow, explicit any_host", "allow", blanketNet, false, ""},
		{"allow, no network declared", "allow", nil, false, ""},
		// Nothing to refuse: the plugin asks for no network at all.
		{"unset field, no network declared", "", nil, false, ""},
		// Unset is deny, so a declared host is refused and the message must not
		// blame a "network: deny" the user never wrote.
		{"unset field, host:port declared", "", structuredNet, true, "не задано"},
		{"unset field, any_host declared", "", blanketNet, true, "не задано"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf := netPipeline(tc.network)
			_, err := Run(pf, netEngine{net: tc.net}, RunOptions{Quiet: true, RunsDir: t.TempDir()})
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if tc.wantDeny {
				if err == nil {
					t.Fatal("expected the network gate to refuse, but the run proceeded")
				}
				if !contains(msg, tc.marker) {
					t.Fatalf("expected refusal containing %q, got: %v", tc.marker, err)
				}
				return
			}
			// The gate let it through: nothing about network may appear in the
			// error. A later runtime failure is fine and not what is asserted.
			if contains(msg, "network") {
				t.Fatalf("the network gate should not have fired, got: %v", err)
			}
		})
	}
}
