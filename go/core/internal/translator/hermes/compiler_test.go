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
		{name: "provider", model: v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderAnthropic, Model: "claude"}, wantErr: "does not support ModelConfig provider"},
		{name: "responses format", model: openRouter(v1alpha3.ModelConfigSpec{OpenAI: &v1alpha3.OpenAIConfig{APIFormat: &responses}}), wantErr: "chatCompletions"},
		{name: "OpenAI options", model: openRouter(v1alpha3.ModelConfigSpec{OpenAI: &v1alpha3.OpenAIConfig{Temperature: "0.2"}}), wantErr: "beyond baseUrl"},
		{name: "base URL", model: openRouter(v1alpha3.ModelConfigSpec{OpenAI: &v1alpha3.OpenAIConfig{BaseURL: "openrouter.ai"}}), wantErr: "absolute HTTP(S) URL"},
		{name: "headers", model: openRouter(v1alpha3.ModelConfigSpec{DefaultHeaders: map[string]string{"X": "y"}}), wantErr: "defaultHeaders"},
		{name: "missing secret key", model: openRouter(v1alpha3.ModelConfigSpec{APIKeySecret: "model-auth"}), wantErr: "apiKeySecretKey"},
		{name: "empty secret", model: openRouter(v1alpha3.ModelConfigSpec{APIKeySecret: "model-auth", APIKeySecretKey: "api-key"}), secret: map[string][]byte{}, wantErr: "non-empty key"},
		{name: "MCP tools", model: openRouter(v1alpha3.ModelConfigSpec{}), mutate: func(input *translator.HarnessInput) {
			input.Root.MCPTools = []translator.ResolvedMCPTool{{}}
		}, wantErr: "does not support tools"},
		{name: "Shared agents", model: openRouter(v1alpha3.ModelConfigSpec{}), mutate: func(input *translator.HarnessInput) {
			input.Root.Shared = []translator.AgentInputBinding{{Name: "child"}}
		}, wantErr: "does not support tools"},
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
