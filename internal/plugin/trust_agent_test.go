package plugin

import (
	"context"
	"runtime"
	"testing"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

func TestTrustPolicyAgentFlags(t *testing.T) {
	policy := TrustPolicy{
		AgentCanExec:         true,
		AgentCanWritePlugins: true,
	}
	ctx := WithTrustPolicy(context.Background(), policy)
	got := TrustPolicyFrom(ctx)
	if !got.AgentCanExec {
		t.Error("AgentCanExec не передался через context")
	}
	if !got.AgentCanWritePlugins {
		t.Error("AgentCanWritePlugins не передался через context")
	}
}

func TestTrustPolicyAgentCanExecFalseByDefault(t *testing.T) {
	policy := TrustPolicy{}
	ctx := WithTrustPolicy(context.Background(), policy)
	got := TrustPolicyFrom(ctx)
	if got.AgentCanExec {
		t.Error("AgentCanExec должен быть false по умолчанию")
	}
	if got.AgentCanWritePlugins {
		t.Error("AgentCanWritePlugins должен быть false по умолчанию")
	}
}

func TestTrustPolicyAgentCanExecDoesNotAffectUntrusted(t *testing.T) {
	policy := TrustPolicy{
		AgentCanExec: true,
	}
	m := &Manifest{Sandbox: pipeline.SandboxUntrusted}
	result := enforceTrust(m, policy)
	if result == nil {
		t.Error("AgentCanExec не должен обходить проверку untrusted")
	}
}

func TestIsAgentWrittenPlugin(t *testing.T) {
	m := &Manifest{Dir: "/work/agent-plugins/my-plugin"}
	if !IsAgentWrittenPlugin(m) {
		t.Error("IsAgentWrittenPlugin должен возвращать true для agent-plugins/")
	}
}

func TestIsAgentWrittenPluginFalse(t *testing.T) {
	m := &Manifest{Dir: "/work/plugins/community/my-plugin"}
	if IsAgentWrittenPlugin(m) {
		t.Error("IsAgentWrittenPlugin должен возвращать false для обычных плагинов")
	}
}

// Регрессия: сверка по подстроке ошибалась в обе стороны. Случаи с
// agent-plugins как ЧАСТЬЮ имени компонента обязаны быть обычными
// плагинами, а на Windows другой регистр — это тот же каталог, значит
// агентский.
func TestIsAgentWrittenPluginComparesPathComponents(t *testing.T) {
	if runtime.GOOS == "windows" {
		cases := []struct {
			dir  string
			want bool
		}{
			{`C:\work\agent-plugins\mailer`, true},
			{`C:\work\Agent-Plugins\mailer`, true}, // на Windows тот же каталог
			{`C:\work\AGENT-PLUGINS\mailer`, true}, // и этот тоже
			{`C:\work\plugins\agent-plugins-x\evil`, false},
			{`C:\work\plugins\mailer-agent-plugins`, false},
			{`C:\work\plugins\mailer`, false},
		}
		for _, c := range cases {
			if got := IsAgentWrittenPlugin(&Manifest{Dir: c.dir}); got != c.want {
				t.Errorf("IsAgentWrittenPlugin(%q) = %v, хотели %v", c.dir, got, c.want)
			}
		}
		return
	}
	cases := []struct {
		dir  string
		want bool
	}{
		{"/work/agent-plugins/mailer", true},
		{"/work/agent-plugins/nested/mailer", true},
		{"/work/Agent-Plugins/mailer", false}, // на POSIX это ДРУГОЙ каталог
		{"/work/plugins/agent-plugins-x/evil", false},
		{"/work/plugins/mailer-agent-plugins", false},
		{"/work/plugins/mailer", false},
	}
	for _, c := range cases {
		if got := IsAgentWrittenPlugin(&Manifest{Dir: c.dir}); got != c.want {
			t.Errorf("IsAgentWrittenPlugin(%q) = %v, хотели %v", c.dir, got, c.want)
		}
	}
}

// Направление ошибки выбрано в сторону безопасности: путь, который реально
// проходит через каталог агента, считается агентским (то есть untrusted).
// Компонента, лишь похожая на agent-plugins, агентской НЕ является — это уже
// проверяет TestIsAgentWrittenPluginComparesPathComponents.
func TestIsAgentWrittenPluginErrsTowardUntrusted(t *testing.T) {
	for _, dir := range []string{
		"/work/backups/agent-plugins/mailer",
		"/work/agent-plugins/nested/mailer",
	} {
		if !IsAgentWrittenPlugin(&Manifest{Dir: dir}) {
			t.Errorf("%q проходит через каталог %s и должен считаться агентским", dir, AgentPluginDir)
		}
	}
}

func TestAgentPluginDirConstant(t *testing.T) {
	if AgentPluginDir != "agent-plugins" {
		t.Error("AgentPluginDir должен быть 'agent-plugins'")
	}
}
