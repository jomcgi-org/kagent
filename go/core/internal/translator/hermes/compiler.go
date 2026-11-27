// Package hermes compiles resolved v1alpha3 inputs for the Hermes Agent
// Harness adapter.
package hermes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2apb/v1/pbconv"
	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	hermesconfig "github.com/kagent-dev/kagent/go/harness/hermes/config"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

// Compiler translates resolved inputs into a Hermes runtime revision.
//
// The first Hermes adapter runs Hermes with every toolset off, so the
// compiler accepts one OpenAI-compatible model and rejects tools, skills,
// plugins and Shared agents rather than dropping them silently.
type Compiler struct {
	settings    *configv1.RuntimeDefaultsConfig
	ctx         krt.HandlerContext
	collections translator.Collections
}

var _ translator.HarnessCompiler = (*Compiler)(nil)

func NewCompiler(ctx krt.HandlerContext, collections translator.Collections, settings *configv1.RuntimeDefaultsConfig) *Compiler {
	return &Compiler{settings: settings, ctx: ctx, collections: collections}
}

func (c *Compiler) Compile(ctx context.Context, input *translator.HarnessInput) (*translator.CompileResult, error) {
	if input == nil || input.Harness == nil || input.Root == nil || input.Root.Template == nil || input.Root.ResolvedModelConfig == nil || input.Root.ResolvedModelConfig.Config == nil {
		return nil, fmt.Errorf("hermes compiler requires a resolved Harness, AgentTemplate, and ModelConfig")
	}
	template, harness := input.Root.Template, input.Harness
	if input.OutputSchema != nil {
		return nil, translator.NewValidationError("Hermes does not support structured output")
	}
	if len(input.Root.MCPTools) != 0 || len(input.Root.Shared) != 0 || len(template.Spec.Tools) != 0 || len(template.Spec.Skills) != 0 || len(template.Spec.Plugins) != 0 {
		return nil, translator.NewValidationError("Hermes does not support tools, skills, plugins, or Shared agents")
	}
	model := input.Root.ResolvedModelConfig.Config
	if strings.TrimSpace(model.Spec.Model) == "" {
		return nil, translator.NewValidationError("Hermes ModelConfig model is required")
	}
	if len(model.Spec.DefaultHeaders) != 0 || !model.Spec.TLS.IsEmpty() || ptr.Deref(model.Spec.TokenPassthrough, false) {
		return nil, translator.NewValidationError("Hermes does not support ModelConfig defaultHeaders, TLS, or tokenPassthrough")
	}
	baseURL, environment, destination, err := c.compileProvider(ctx, input.Root.ResolvedModelConfig)
	if err != nil {
		return nil, err
	}
	apiMode, cacheTTL := "", ""
	if model.Spec.Provider == v1alpha3.ModelProviderAnthropic {
		apiMode, cacheTTL = hermesconfig.APIModeAnthropicMessages, "off"
		if a := model.Spec.Anthropic; a != nil && a.PromptCaching {
			cacheTTL = a.CacheTTL
			if cacheTTL == "" {
				cacheTTL = "5m"
			}
		}
	}
	for _, variable := range harness.Spec.Env {
		if hermesconfig.OwnsEnvironment(variable.Name) {
			return nil, translator.NewValidationError("Harness env %q conflicts with Hermes's compiled configuration", variable.Name)
		}
		environment = append(environment, corev1.EnvVar{Name: variable.Name, Value: variable.Value})
	}
	telemetryConfig, err := translator.TelemetryConfigFromSettings(c.settings.GetTelemetry())
	if err != nil {
		return nil, err
	}

	cfg := hermesconfig.Production(strings.TrimSpace(model.Spec.Model), baseURL, input.Root.Instruction)
	cfg.APIMode, cfg.CacheTTL = apiMode, cacheTTL
	if compression := harness.Spec.Hermes.Compression; compression != nil {
		cfg.CompressionThresholdTokens = int(ptr.Deref(compression.ThresholdTokens, 0))
		cfg.CompressionMinTailUserMessages = int(ptr.Deref(compression.MinTailUserMessages, 0))
		cfg.CompressionModel = strings.TrimSpace(compression.Model)
	}
	cfg.RuntimeTelemetry = telemetryConfig.RuntimeTelemetry(tracing.RuntimeHermes, input.AgentName, template.Namespace, model.Spec)
	cfg.ControlPlaneURL = c.settings.GetControlPlaneUrl()
	cfg.MaxContentLength = c.settings.MaxContentLength // Preserve unset versus unlimited.
	cfg.LogLevel = c.settings.GetLogging().GetLevel()
	if err := cfg.Validate(); err != nil {
		return nil, translator.NewValidationError("invalid compiled Hermes configuration: %v", err)
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal Hermes config: %w", err)
	}
	card := translator.ManagedAgentCard(input.AgentName, template)
	// Hermes denies approvals instead of parking a turn, so the runtime never
	// asks for input.
	card.Capabilities.Extensions = nil
	protoCard, err := pbconv.ToProtoAgentCard(card)
	if err != nil {
		return nil, fmt.Errorf("convert Hermes agent card: %w", err)
	}
	provenance, err := c.buildProvenance(input)
	if err != nil {
		return nil, fmt.Errorf("build Hermes revision provenance: %w", err)
	}
	environment, credentials, tokenPassthroughTargets, err := translator.CompileCredentials(input, nil, environment)
	if err != nil {
		return nil, err
	}
	destinations := []string{destination, c.settings.GetControlPlaneUrl()}
	destinations = append(destinations, telemetryConfig.Destinations(environment)...)
	slices.Sort(destinations)
	return &translator.CompileResult{Revision: translator.Revision{
		ControlPlaneURL: c.settings.GetControlPlaneUrl(),
		Namespace:       template.Namespace,
		Environment:     environment, ConfigJSON: configJSON, AgentCard: protoCard,
		WorkerPoolName: harness.Spec.Substrate.WorkerPoolRef.Name, SnapshotLocation: harness.Spec.Substrate.SnapshotPolicy.Location,
		TokenPassthroughTargets: tokenPassthroughTargets, Credentials: credentials, Provenance: provenance,
		EgressDestinations: slices.Compact(destinations),
	}}, nil
}

// compileProvider accepts an OpenAI-compatible ModelConfig, which Hermes
// calls as its named provider through chat completions, or an Anthropic one,
// which it calls through the Messages API.
func (c *Compiler) compileProvider(ctx context.Context, resolved *translator.ResolvedModelConfig) (baseURL string, environment []corev1.EnvVar, destination string, err error) {
	model := resolved.Config
	switch model.Spec.Provider {
	case v1alpha3.ModelProviderOpenAI:
	case v1alpha3.ModelProviderAnthropic:
		return c.compileAnthropic(ctx, resolved)
	default:
		return "", nil, "", translator.NewValidationError("Hermes does not support ModelConfig provider %q", model.Spec.Provider)
	}
	options := v1alpha3.OpenAIConfig{}
	if model.Spec.OpenAI != nil {
		options = *model.Spec.OpenAI
	}
	if options.APIFormat != nil && *options.APIFormat != v1alpha3.OpenAIAPIFormatChatCompletions {
		return "", nil, "", translator.NewValidationError("Hermes OpenAI requires openAI.apiFormat chatCompletions or none")
	}
	baseURL = strings.TrimSpace(options.BaseURL)
	options.BaseURL, options.APIFormat = "", nil
	if !reflect.DeepEqual(options, v1alpha3.OpenAIConfig{}) {
		return "", nil, "", translator.NewValidationError("Hermes does not support OpenAI provider options beyond baseUrl and apiFormat")
	}
	if baseURL == "" {
		baseURL = hermesconfig.DefaultBaseURL
	}
	parsed, parseErr := url.Parse(baseURL)
	if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", nil, "", translator.NewValidationError("Hermes OpenAI baseUrl must be an absolute HTTP(S) URL without credentials or fragment")
	}
	destination = egress.Origin(parsed)
	switch {
	case model.Spec.ActorJWT != nil:
		credential, err := translator.ActorJWTEnvironment(resolved)
		if err != nil {
			return "", nil, "", err
		}
		environment = []corev1.EnvVar{credential}
	case model.Spec.APIKeySecret != "":
		if model.Spec.APIKeySecretKey == "" {
			return "", nil, "", translator.NewValidationError("Hermes OpenAI requires apiKeySecretKey with apiKeySecret")
		}
		if err := c.requireSecretKey(ctx, model.Namespace, model.Spec.APIKeySecret, model.Spec.APIKeySecretKey); err != nil {
			return "", nil, "", err
		}
		// CompileCredentials binds the key to the egress gateway and leaves the
		// Actor only a placeholder.
		environment = []corev1.EnvVar{{Name: hermesconfig.APIKeyEnvName, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: model.Spec.APIKeySecret}, Key: model.Spec.APIKeySecretKey,
		}}}}
	}
	return baseURL, environment, destination, nil
}

// compileAnthropic accepts an Anthropic ModelConfig with an API key Secret.
// Hermes sends the placeholder as x-api-key, and kagent binds that header
// at the egress gateway.
func (c *Compiler) compileAnthropic(ctx context.Context, resolved *translator.ResolvedModelConfig) (baseURL string, environment []corev1.EnvVar, destination string, err error) {
	model := resolved.Config
	options := v1alpha3.AnthropicConfig{}
	if model.Spec.Anthropic != nil {
		options = *model.Spec.Anthropic
	}
	baseURL = strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	options.BaseURL, options.PromptCaching, options.CacheTTL = "", false, ""
	if !reflect.DeepEqual(options, v1alpha3.AnthropicConfig{}) {
		return "", nil, "", translator.NewValidationError("Hermes does not support Anthropic provider options beyond baseUrl, promptCaching and cacheTTL")
	}
	if baseURL == "" {
		baseURL = hermesconfig.DefaultAnthropicBaseURL
	}
	parsed, parseErr := url.Parse(baseURL)
	if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", nil, "", translator.NewValidationError("Hermes Anthropic baseUrl must be an absolute HTTP(S) URL without credentials or fragment")
	}
	destination = egress.Origin(parsed)
	if model.Spec.ActorJWT != nil {
		return "", nil, "", translator.NewValidationError("Hermes Anthropic does not support actorJwt")
	}
	if model.Spec.APIKeySecret != "" {
		if model.Spec.APIKeySecretKey == "" {
			return "", nil, "", translator.NewValidationError("Hermes Anthropic requires apiKeySecretKey with apiKeySecret")
		}
		if err := c.requireSecretKey(ctx, model.Namespace, model.Spec.APIKeySecret, model.Spec.APIKeySecretKey); err != nil {
			return "", nil, "", err
		}
		environment = []corev1.EnvVar{{Name: hermesconfig.AnthropicAPIKeyEnvName, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: model.Spec.APIKeySecret}, Key: model.Spec.APIKeySecretKey,
		}}}}
	}
	return baseURL, environment, destination, nil
}

func (c *Compiler) requireSecretKey(_ context.Context, namespace, name, key string) error {
	secret := krt.FetchOne(c.ctx, c.collections.Secrets, krt.FilterObjectName(types.NamespacedName{Namespace: namespace, Name: name}))
	if secret == nil {
		return fmt.Errorf("read Hermes credential Secret %q: not found", name)
	}
	if len((*secret).Data[key]) == 0 {
		return translator.NewValidationError("Hermes credential Secret %q does not contain a non-empty key %q", name, key)
	}
	return nil
}

type provenanceEntry struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation,omitempty"`
	Hash       string    `json:"hash"`
}

// buildProvenance records every object the revision reads: the Harness, the
// AgentTemplate, the ModelConfig, and the ConfigMaps behind the prompt.
func (c *Compiler) buildProvenance(input *translator.HarnessInput) ([]byte, error) {
	entries := []provenanceEntry{objectProvenance("Harness", input.Harness.Name, input.Harness.Source.UID, input.Harness.Source.Generation, input.Harness.Spec)}
	if source := input.Root.Template.Source; source != nil {
		entries = append(entries, objectProvenance("AgentTemplate", source.Name, source.UID, source.Generation, input.Root.Template.Spec))
	}
	model := input.Root.ResolvedModelConfig.Config
	entries = append(entries, objectProvenance("ModelConfig", model.Namespace+"/"+model.Name, model.UID, model.Generation, model.Spec))
	configMaps := map[types.NamespacedName]struct{}{}
	spec := input.Root.Template.Spec
	if spec.SystemPromptFrom != nil {
		configMaps[types.NamespacedName{Namespace: input.Harness.Namespace, Name: spec.SystemPromptFrom.Name}] = struct{}{}
	}
	if spec.PromptTemplate != nil {
		for _, source := range spec.PromptTemplate.DataSources {
			configMaps[types.NamespacedName{Namespace: input.Harness.Namespace, Name: source.Name}] = struct{}{}
		}
	}
	for name := range configMaps {
		configMap := krt.FetchOne(c.ctx, c.collections.ConfigMaps, krt.FilterObjectName(name))
		if configMap == nil {
			return nil, fmt.Errorf("ConfigMap %q not found", name)
		}
		entry := objectProvenance("ConfigMap", name.String(), (*configMap).UID, (*configMap).Generation, (*configMap).Data)
		entry.APIVersion = "v1"
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b provenanceEntry) int {
		return strings.Compare(a.APIVersion+"\x00"+a.Kind+"\x00"+a.Name, b.APIVersion+"\x00"+b.Kind+"\x00"+b.Name)
	})
	return json.Marshal(entries)
}

func objectProvenance(kind, name string, uid types.UID, generation int64, content any) provenanceEntry {
	raw, _ := json.Marshal(content)
	hash := sha256.Sum256(raw)
	return provenanceEntry{APIVersion: v1alpha3.GroupVersion.String(), Kind: kind, Name: name, UID: uid, Generation: generation, Hash: fmt.Sprintf("%x", hash[:])}
}
