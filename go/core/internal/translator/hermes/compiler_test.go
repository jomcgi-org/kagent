package hermes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/krt/krttest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/kagent-dev/kagent/go/core/pkg/env"
	hermesconfig "github.com/kagent-dev/kagent/go/harness/hermes/config"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

const credentialValue = "credential-must-not-be-serialized"

func testRuntimeDefaults() *configv1.RuntimeDefaultsConfig {
	return &configv1.RuntimeDefaultsConfig{ControlPlaneUrl: new("http://kagent-controller.kagent:8083"), GatewayUrl: new("http://kagent-controller.kagent:8083"), Logging: &configv1.LoggingConfig{Level: new("info")}}
}

func testInput(t *testing.T, modelSpec v1alpha3.ModelConfigSpec, secretData map[string][]byte) (*translator.HarnessInput, translator.Collections) {
	t.Helper()
	harness := &translator.HarnessConfiguration{Name: "hermes", Namespace: "test", Source: metav1.ObjectMeta{Name: "hermes", Namespace: "test", UID: "harness"}, Spec: v1alpha3.HarnessSpec{
		Hermes: &v1alpha3.HermesHarness{}, Workload: v1alpha3.HarnessWorkload{Image: new("example.com/hermes@sha256:" + strings.Repeat("a", 64))},
		Substrate: v1alpha3.RuntimeSubstratePolicy{WorkerPoolRef: corev1.LocalObjectReference{Name: "default"}, SnapshotPolicy: v1alpha3.RuntimeSnapshotPolicy{Location: "snapshots"}},
	}}
	template := &translator.TemplateConfiguration{Name: "assistant", Namespace: "test", Source: &metav1.ObjectMeta{Name: "assistant", Namespace: "test", UID: "template"}, Spec: v1alpha3.AgentTemplateSpec{ModelConfig: &v1alpha3.ObjectReference{Name: "model"}, Description: "assistant"}}
	model := &v1alpha3.ModelConfig{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "test", UID: "model"}, Spec: modelSpec}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "model-auth", Namespace: "test", UID: "secret"}, Data: secretData}
	mock := krttest.NewMock(t, []any{secret})
	collections := translator.Collections{
		Secrets:    krttest.GetMockCollection[*corev1.Secret](mock),
		ConfigMaps: krttest.GetMockCollection[*corev1.ConfigMap](mock),
	}
	return &translator.HarnessInput{AgentName: "runnable-agent", Harness: harness, Root: &translator.AgentInput{
		Template: template, ResolvedModelConfig: &translator.ResolvedModelConfig{Config: model}, Instruction: "help carefully",
	}}, collections
}

func openRouter(spec v1alpha3.ModelConfigSpec) v1alpha3.ModelConfigSpec {
	spec.Provider, spec.Model = v1alpha3.ModelProviderOpenAI, "deepseek/deepseek-v4.1-flash"
	if spec.OpenAI == nil {
		spec.OpenAI = &v1alpha3.OpenAIConfig{BaseURL: "https://openrouter.ai/api/v1"}
	}
	return spec
}

func TestCompileOpenRouterWithGatewayCredential(t *testing.T) {
	input, collections := testInput(t, openRouter(v1alpha3.ModelConfigSpec{APIKeySecret: "model-auth", APIKeySecretKey: "api-key"}), map[string][]byte{"api-key": []byte(credentialValue)})
	result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	revision := result.Revision
	cfg, err := hermesconfig.Parse(revision.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "deepseek/deepseek-v4.1-flash" || cfg.BaseURL != "https://openrouter.ai/api/v1" || cfg.Instructions != "help carefully" {
		t.Fatalf("config = %#v", cfg)
	}
	if cfg.ControlPlaneURL != "http://kagent-controller.kagent:8083" || cfg.LogLevel != "info" {
		t.Fatalf("control plane settings = %q %q", cfg.ControlPlaneURL, cfg.LogLevel)
	}
	want := tracing.RuntimeTelemetry{Runtime: tracing.RuntimeHermes, AgentName: "runnable-agent", AgentNamespace: "test", Provider: "openai", Model: "deepseek/deepseek-v4.1-flash"}
	if cfg.RuntimeTelemetry != want {
		t.Fatalf("runtime telemetry = %#v, want %#v", cfg.RuntimeTelemetry, want)
	}
	if strings.Contains(string(revision.ConfigJSON), credentialValue) {
		t.Fatal("config contains the credential")
	}
	var found bool
	for _, variable := range revision.Environment {
		if variable.ValueFrom != nil || strings.Contains(variable.Value, credentialValue) {
			t.Fatalf("environment %q leaks a secret reference or value", variable.Name)
		}
		if variable.Name == hermesconfig.APIKeyEnvName {
			found = variable.Value == translator.CredentialPlaceholder
		}
	}
	if !found {
		t.Fatalf("environment lacks the credential placeholder: %#v", revision.Environment)
	}
	if len(revision.Credentials) != 1 || revision.Credentials[0].Origin != "https://openrouter.ai:443" || revision.Credentials[0].Header != "authorization" || revision.Credentials[0].Prefix != "Bearer " {
		t.Fatalf("credentials = %#v", revision.Credentials)
	}
	if got := strings.Join(revision.EgressDestinations, ","); got != "http://kagent-controller.kagent:8083,https://openrouter.ai:443" {
		t.Fatalf("egress = %s", got)
	}
	if revision.WorkerPoolName != "default" || revision.SnapshotLocation != "snapshots" {
		t.Fatalf("substrate = %q %q", revision.WorkerPoolName, revision.SnapshotLocation)
	}
	var card struct {
		Capabilities struct {
			Extensions []any `json:"extensions"`
		} `json:"capabilities"`
	}
	raw, err := json.Marshal(revision.AgentCard)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &card); err != nil || len(card.Capabilities.Extensions) != 0 {
		t.Fatalf("agent card advertises input requests: %s", raw)
	}
}

func TestCompileWithoutCredentialUsesDefaultOpenAIEndpoint(t *testing.T) {
	input, collections := testInput(t, v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5.2"}, nil)
	result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := hermesconfig.Parse(result.Revision.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != hermesconfig.DefaultBaseURL || len(result.Revision.Credentials) != 0 {
		t.Fatalf("base URL %q, credentials %#v", cfg.BaseURL, result.Revision.Credentials)
	}
}

func TestCompileRejectsUnsupportedInputs(t *testing.T) {
	responses := v1alpha3.OpenAIAPIFormatResponses
	tests := []struct {
		name    string
		model   v1alpha3.ModelConfigSpec
		secret  map[string][]byte
		mutate  func(*translator.HarnessInput)
		wantErr string
	}{
		{name: "provider", model: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderGemini, Model: "gemini"}, wantErr: "does not support ModelConfig provider"},
		{name: "responses format", model: openRouter(v1alpha3.ModelConfigSpec{OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses}}), wantErr: "chatCompletions"},
		{name: "OpenAI options", model: openRouter(v1alpha3.ModelConfigSpec{OpenAI: &v1alpha3.OpenAIConfig{Temperature: "0.2"}}), wantErr: "beyond baseUrl"},
		{name: "base URL", model: openRouter(v1alpha3.ModelConfigSpec{OpenAI: &v1alpha3.OpenAIConfig{BaseURL: "openrouter.ai"}}), wantErr: "absolute HTTP(S) URL"},
		{name: "headers", model: openRouter(v1alpha3.ModelConfigSpec{DefaultHeaders: map[string]string{"X": "y"}}), wantErr: "defaultHeaders"},
		{name: "missing secret key", model: openRouter(v1alpha3.ModelConfigSpec{APIKeySecret: "model-auth"}), wantErr: "apiKeySecretKey"},
		{name: "empty secret", model: openRouter(v1alpha3.ModelConfigSpec{APIKeySecret: "model-auth", APIKeySecretKey: "api-key"}), secret: map[string][]byte{}, wantErr: "non-empty key"},
		{name: "MCP tools", model: openRouter(v1alpha3.ModelConfigSpec{}), mutate: func(input *translator.HarnessInput) {
			input.Root.MCPTools = []translator.ResolvedMCPTool{{}}
		}, wantErr: "supports only RemoteMCPServer tools"},
		{name: "sub-agent tools", model: openRouter(v1alpha3.ModelConfigSpec{}), mutate: func(input *translator.HarnessInput) {
			input.Root.Template.Spec.Tools = []v1alpha3.ToolBinding{{SubAgent: &v1alpha3.SubAgentToolBinding{Name: "child", Description: "child"}}}
		}, wantErr: "supports only RemoteMCPServer tools"},
		{name: "server tools on OpenAI", model: openRouter(v1alpha3.ModelConfigSpec{}), mutate: func(input *translator.HarnessInput) {
			input.Harness.Spec.Hermes.ServerTools = &v1alpha3.HermesServerTools{CodeExecution: true}
		}, wantErr: "Anthropic provider"},
		{name: "Shared agents", model: openRouter(v1alpha3.ModelConfigSpec{}), mutate: func(input *translator.HarnessInput) {
			input.Root.Shared = []translator.AgentInputBinding{{Name: "child"}}
		}, wantErr: "supports only RemoteMCPServer tools"},
		{name: "structured output", model: openRouter(v1alpha3.ModelConfigSpec{}), mutate: func(input *translator.HarnessInput) {
			input.OutputSchema = &translator.ResolvedOutputSchema{}
		}, wantErr: "structured output"},
		{name: "reserved env", model: openRouter(v1alpha3.ModelConfigSpec{}), mutate: func(input *translator.HarnessInput) {
			input.Harness.Spec.Env = []v1alpha3.RuntimeEnvVar{{Name: "HERMES_YOLO_MODE", Value: "1"}}
		}, wantErr: "conflicts with Hermes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, collections := testInput(t, test.model, test.secret)
			if test.mutate != nil {
				test.mutate(input)
			}
			_, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Compile() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestCompilePassesHarnessEnvironment(t *testing.T) {
	input, collections := testInput(t, openRouter(v1alpha3.ModelConfigSpec{}), nil)
	input.Harness.Spec.Env = []v1alpha3.RuntimeEnvVar{{Name: "TZ", Value: "Europe/London"}}
	result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, variable := range result.Revision.Environment {
		if variable.Name == "TZ" && variable.Value == "Europe/London" {
			return
		}
	}
	t.Fatalf("environment = %#v", result.Revision.Environment)
}

func TestCompilePassesCompressionSettings(t *testing.T) {
	input, collections := testInput(t, openRouter(v1alpha3.ModelConfigSpec{}), nil)
	input.Harness.Spec.Hermes.Compression = &v1alpha3.HermesCompression{ThresholdTokens: new(int32(128000)), MinTailUserMessages: new(int32(3)), Model: "deepseek/deepseek-v4.1-flash-1017"}
	result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := hermesconfig.Parse(result.Revision.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CompressionThresholdTokens != 128000 || cfg.CompressionMinTailUserMessages != 3 || cfg.CompressionModel != "deepseek/deepseek-v4.1-flash-1017" {
		t.Fatalf("compression = %d %d %q", cfg.CompressionThresholdTokens, cfg.CompressionMinTailUserMessages, cfg.CompressionModel)
	}
	// The summary model shares the main model's origin, so egress is unchanged.
	if got := strings.Join(result.Revision.EgressDestinations, ","); got != "http://kagent-controller.kagent:8083,https://openrouter.ai:443" {
		t.Fatalf("egress = %s", got)
	}
}

func TestCompileWithoutCompressionOmitsSettings(t *testing.T) {
	input, collections := testInput(t, openRouter(v1alpha3.ModelConfigSpec{}), nil)
	result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Revision.ConfigJSON), "compression") {
		t.Fatalf("config names compression: %s", result.Revision.ConfigJSON)
	}
}

func TestCompileAnthropicUsesMessagesAndTheGatewayKey(t *testing.T) {
	if hermesconfig.AnthropicAPIKeyEnvName != env.AnthropicAPIKey.Name() {
		t.Fatalf("Hermes Anthropic key env %q, kagent binds %q", hermesconfig.AnthropicAPIKeyEnvName, env.AnthropicAPIKey.Name())
	}
	spec := v1alpha3.ModelConfigSpec{
		Provider: v1alpha3.ModelProviderAnthropic, Model: "claude-opus-5-5", APIKeySecret: "model-auth", APIKeySecretKey: "api-key",
		Anthropic: &v1alpha3.AnthropicConfig{BaseURL: "https://api.anthropic.com", PromptCaching: true, CacheTTL: "1h"},
	}
	input, collections := testInput(t, spec, map[string][]byte{"api-key": []byte(credentialValue)})
	result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	revision := result.Revision
	cfg, err := hermesconfig.Parse(revision.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIMode != hermesconfig.APIModeAnthropicMessages || cfg.CacheTTL != "1h" || cfg.BaseURL != "https://api.anthropic.com" || cfg.APIKeyEnv() != "ANTHROPIC_API_KEY" {
		t.Fatalf("config = %#v", cfg)
	}
	if strings.Contains(string(revision.ConfigJSON), credentialValue) {
		t.Fatal("config contains the credential")
	}
	var found bool
	for _, variable := range revision.Environment {
		if variable.ValueFrom != nil || strings.Contains(variable.Value, credentialValue) {
			t.Fatalf("environment %q leaks a secret reference or value", variable.Name)
		}
		if variable.Name == hermesconfig.AnthropicAPIKeyEnvName {
			found = variable.Value == translator.CredentialPlaceholder
		}
	}
	if !found {
		t.Fatalf("environment lacks the Anthropic placeholder: %#v", revision.Environment)
	}
	if len(revision.Credentials) != 1 || revision.Credentials[0].Origin != "https://api.anthropic.com:443" || revision.Credentials[0].Header != "x-api-key" || revision.Credentials[0].Prefix != "" {
		t.Fatalf("credentials = %#v", revision.Credentials)
	}
	if got := strings.Join(revision.EgressDestinations, ","); got != "http://kagent-controller.kagent:8083,https://api.anthropic.com:443" {
		t.Fatalf("egress = %s", got)
	}
}

func TestCompileAnthropicCachingDefaultsAndRejectsOtherOptions(t *testing.T) {
	for _, test := range []struct {
		options *v1alpha3.AnthropicConfig
		ttl     string
	}{
		{nil, "off"},
		{&v1alpha3.AnthropicConfig{PromptCaching: true}, "5m"},
	} {
		input, collections := testInput(t, v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderAnthropic, Model: "claude-opus-5-5", Anthropic: test.options}, nil)
		result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := hermesconfig.Parse(result.Revision.ConfigJSON)
		if err != nil || cfg.CacheTTL != test.ttl || cfg.BaseURL != hermesconfig.DefaultAnthropicBaseURL {
			t.Fatalf("config = %#v, %v; want cache_ttl %q", cfg, err, test.ttl)
		}
	}
	input, collections := testInput(t, v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderAnthropic, Model: "claude-opus-5-5", Anthropic: &v1alpha3.AnthropicConfig{MaxTokens: 10}}, nil)
	if _, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input); err == nil || !strings.Contains(err.Error(), "beyond baseUrl") {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestCompileOpenAIKeepsTheChatCompletionsConfig(t *testing.T) {
	input, collections := testInput(t, openRouter(v1alpha3.ModelConfigSpec{}), nil)
	result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if raw := string(result.Revision.ConfigJSON); strings.Contains(raw, "api_mode") || strings.Contains(raw, "cache_ttl") {
		t.Fatalf("OpenAI config names Anthropic settings: %s", raw)
	}
}
