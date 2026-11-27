/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// KagentHarness configures the kagent runtime adapter.
type KagentHarness struct {
	// Memory enables long-term memory for agents using this Harness.
	// +optional
	Memory *KagentHarnessMemory `json:"memory,omitempty"`

	// Compaction summarizes older session events so the prompt stays small as
	// a conversation grows. Omitted leaves the history uncompacted.
	// +optional
	Compaction *KagentHarnessCompaction `json:"compaction,omitempty"`
}

// KagentHarnessCompaction selects the compaction strategies and the model that
// writes the summaries. The sliding window (compactionInterval, overlapSize)
// summarizes each group of completed invocations; tail retention
// (tokenThreshold, eventRetentionSize) bounds the prompt by summarizing
// everything but the most recent events once the prompt grows past a token
// count. At least one strategy must be configured.
// +kubebuilder:validation:AtLeastOneOf=compactionInterval;tokenThreshold
// +kubebuilder:validation:XValidation:rule="has(self.tokenThreshold) == has(self.eventRetentionSize)",message="tokenThreshold and eventRetentionSize must be specified together"
// +kubebuilder:validation:XValidation:rule="!has(self.overlapSize) || has(self.compactionInterval)",message="overlapSize requires compactionInterval"
type KagentHarnessCompaction struct {
	// CompactionInterval is the number of new user-initiated invocations that,
	// once fully represented in the session, triggers a sliding-window
	// compaction of those invocations.
	// +kubebuilder:validation:Minimum=1
	// +optional
	CompactionInterval *int `json:"compactionInterval,omitempty"`
	// OverlapSize is the number of already-compacted invocations pulled back
	// into the next sliding window so consecutive summaries overlap.
	// +kubebuilder:validation:Minimum=0
	// +optional
	OverlapSize *int `json:"overlapSize,omitempty"`
	// TokenThreshold is the prompt token count at which tail-retention
	// compaction summarizes the history before the next model call.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TokenThreshold *int `json:"tokenThreshold,omitempty"`
	// EventRetentionSize is the number of most recent events that tail
	// retention keeps uncompacted.
	// +kubebuilder:validation:Minimum=1
	// +optional
	EventRetentionSize *int `json:"eventRetentionSize,omitempty"`
	// Summarizer selects the model and prompt that write the summaries.
	// Omitted summarizes with the agent's own model and the runtime's default
	// prompt.
	// +optional
	Summarizer *KagentHarnessSummarizer `json:"summarizer,omitempty"`
}

// KagentHarnessSummarizer configures the model that summarizes compacted events.
// +kubebuilder:validation:XValidation:rule="!has(self.promptTemplate) || self.promptTemplate.contains('{conversation_history}')",message="promptTemplate must contain {conversation_history}"
type KagentHarnessSummarizer struct {
	// ModelConfigRef references the ModelConfig (defaulting to the Harness namespace) that
	// writes the summaries. Omitted uses the agent's own model.
	// +kubebuilder:validation:XValidation:rule="has(self.name) && self.name != ''",message="name must not be empty"
	// +optional
	ModelConfigRef *ObjectReference `json:"modelConfigRef,omitempty"`
	// PromptTemplate replaces the runtime's default summarization prompt. It
	// must contain {conversation_history}, which the runtime replaces with the
	// rendered events.
	// +kubebuilder:validation:MinLength=1
	// +optional
	PromptTemplate string `json:"promptTemplate,omitempty"`
}

// KagentHarnessMemory configures kagent's long-term memory service.
// +kubebuilder:validation:XValidation:rule="self.modelConfigRef.name.size() > 0",message="modelConfigRef name must not be empty"
type KagentHarnessMemory struct {
	// ModelConfigRef references the embedding ModelConfig (defaulting to the Harness namespace).
	// +required
	ModelConfigRef ObjectReference `json:"modelConfigRef"`

	// TTLDays controls how many days a stored memory entry remains valid.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TTLDays int `json:"ttlDays,omitempty"`
}

// CodexHarness selects the Codex runtime adapter.
type CodexHarness struct{}

// ClaudeHarness selects the Claude runtime adapter.
type ClaudeHarness struct{}

// HermesHarness selects the Hermes Agent runtime adapter. Kagent publishes no
// built-in Hermes image, so workload.image must name one that contains Hermes
// Agent and the kagent-hermes adapter.
type HermesHarness struct {
	// Compression tunes Hermes context compression. Unset keeps Hermes's
	// defaults: compress at half the model window, summarised by the main model.
	// +optional
	Compression *HermesCompression `json:"compression,omitempty"`

	// ServerTools adds Anthropic server tools to each Messages API request of
	// a turn. Anthropic runs them; the Actor makes no request to the tool's
	// target. It requires a ModelConfig with the Anthropic provider.
	// +optional
	ServerTools *HermesServerTools `json:"serverTools,omitempty"`
}

// HermesServerTools selects Anthropic server tools. Each unset field leaves
// its tool out.
type HermesServerTools struct {
	// WebSearch adds web_search (type web_search_20260209).
	// +optional
	WebSearch *HermesServerToolLimit `json:"webSearch,omitempty"`

	// WebFetch adds web_fetch (type web_fetch_20260209). It fetches only URLs
	// that are already in the conversation.
	// +optional
	WebFetch *HermesServerToolLimit `json:"webFetch,omitempty"`

	// CodeExecution adds code_execution (type code_execution_20260521). Code
	// runs in Anthropic's sandbox, which has no network access.
	// +optional
	CodeExecution bool `json:"codeExecution,omitempty"`
}

// HermesServerToolLimit bounds one server tool.
type HermesServerToolLimit struct {
	// MaxUses is the tool's max_uses: the most calls in one Messages API
	// request. A turn that calls MCP tools makes one request per model step.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +required
	MaxUses int32 `json:"maxUses"`
}

// HermesCompression sets when Hermes compresses a session and which model
// writes the summary.
type HermesCompression struct {
	// ThresholdTokens compresses the session when its prompt reaches this many
	// tokens, or the default half-window ratio, whichever is lower.
	// +kubebuilder:validation:Minimum=8192
	// +optional
	ThresholdTokens *int32 `json:"thresholdTokens,omitempty"`

	// MinTailUserMessages is how many of the newest user messages a
	// compression keeps verbatim. Unset keeps Hermes's default of 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +optional
	MinTailUserMessages *int32 `json:"minTailUserMessages,omitempty"`

	// Model is the model ID that writes the summary. Hermes calls it through
	// the ModelConfig's endpoint and credential, like the main model. Unset
	// means the main model.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^[:space:]]+$`
	// +optional
	Model string `json:"model,omitempty"`
}

// BYOHarness selects an image that implements kagent's private A2A contract.
type BYOHarness struct{}

// HarnessWorkload identifies the immutable runtime image used by a Harness.
type HarnessWorkload struct {
	// Image overrides the release's built-in runtime image. BYO and Hermes require an image.
	// When set, it must be an OCI image reference pinned by sha256 digest.
	// +kubebuilder:validation:Pattern=`^[^[:space:]@]+@sha256:[a-f0-9]{64}$`
	// +optional
	Image *string `json:"image,omitempty"`

	// Command overrides the image entrypoint when set.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Command []string `json:"command,omitempty"`

	// Args overrides the image command arguments when set.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Args []string `json:"args,omitempty"`
}

// HarnessSpec defines a reusable runtime and its infrastructure policy.
//
// +kubebuilder:validation:ExactlyOneOf=kagent;codex;claude;hermes;byo
// +kubebuilder:validation:XValidation:rule="!has(self.byo) || (has(self.workload) && has(self.workload.command) && size(self.workload.command) > 0)",message="BYO harnesses must specify workload.command"
// +kubebuilder:validation:XValidation:rule="!has(self.byo) || (has(self.workload) && has(self.workload.image))",message="BYO harnesses must specify workload.image"
// +kubebuilder:validation:XValidation:rule="!has(self.hermes) || (has(self.workload) && has(self.workload.image))",message="Hermes harnesses must specify workload.image"
type HarnessSpec struct {
	// +optional
	Kagent *KagentHarness `json:"kagent,omitempty"`

	// +optional
	Codex *CodexHarness `json:"codex,omitempty"`

	// +optional
	Claude *ClaudeHarness `json:"claude,omitempty"`

	// +optional
	Hermes *HermesHarness `json:"hermes,omitempty"`

	// +optional
	BYO *BYOHarness `json:"byo,omitempty"`

	// Workload overrides the built-in runtime image and its command. BYO and Hermes require it.
	// +optional
	Workload HarnessWorkload `json:"workload,omitzero"`

	// +optional
	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=name
	Env []RuntimeEnvVar `json:"env,omitempty"`

	// +required
	Substrate RuntimeSubstratePolicy `json:"substrate"`

	// IdleSuspendDelay keeps a Session's runtime running for this long after
	// its last turn settles, so follow-up turns skip restore and harness
	// startup. A new turn restarts the delay, and a checkpoint of the held
	// turn ends it. Suspension can follow up to one quiescence recovery
	// interval after the delay. Unset or zero suspends at once.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s') && duration(self) <= duration('1h')",message="idleSuspendDelay must be between 0s and 1h"
	// +optional
	IdleSuspendDelay *metav1.Duration `json:"idleSuspendDelay,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=harnesses,singular=harness,categories=kagent
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Harness defines a reusable agent runtime and infrastructure policy.
type Harness struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec HarnessSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// HarnessList contains a list of Harness resources.
type HarnessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Harness `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Harness{}, &HarnessList{})
		return nil
	})
}
