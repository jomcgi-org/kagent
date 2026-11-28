package hermes

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"istio.io/istio/pkg/kube/krt"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	hermesconfig "github.com/kagent-dev/kagent/go/harness/hermes/config"
)

type mcpCompilation struct {
	servers map[string]hermesconfig.MCPServer
	// names maps each bound RemoteMCPServer, as namespace/name, to its
	// Hermes server name.
	names  map[string]string
	egress []string
}

// compileMCP renders each RemoteMCPServer binding as a Hermes mcp_servers
// entry. Hermes registers only the binding's listed tools, so a binding must
// list them. Secret-backed headers and actorJwt hold the credential
// placeholder; CompileCredentials binds the real value at the egress gateway.
func (c *Compiler) compileMCP(namespace string, tools []translator.ResolvedMCPTool) (mcpCompilation, error) {
	if len(tools) == 0 {
		return mcpCompilation{}, nil
	}
	result := mcpCompilation{servers: make(map[string]hermesconfig.MCPServer, len(tools)), names: make(map[string]string, len(tools))}
	for _, tool := range tools {
		server := tool.Server
		if server == nil {
			return mcpCompilation{}, fmt.Errorf("resolved Hermes MCP binding has no server")
		}
		name := translator.MCPRuntimeName(namespace, server)
		if hermesconfig.ReservedMCPServerName(name) {
			return mcpCompilation{}, translator.NewValidationError("Hermes RemoteMCPServer %q maps to Hermes toolset name %q; rename it", server.Name, name)
		}
		if _, ok := result.servers[name]; ok {
			return mcpCompilation{}, translator.NewValidationError("RemoteMCPServer %q is bound more than once or shares Hermes name %q", server.Name, name)
		}
		switch {
		case ptr.Deref(server.Spec.TokenPassthrough, false):
			return mcpCompilation{}, translator.NewValidationError("Hermes does not support RemoteMCPServer %q tokenPassthrough", server.Name)
		case server.Spec.Protocol != "" && server.Spec.Protocol != v1alpha3.RemoteMCPServerProtocolStreamableHttp:
			return mcpCompilation{}, translator.NewValidationError("Hermes RemoteMCPServer %q requires Streamable HTTP", server.Name)
		case !server.Spec.TLS.IsEmpty():
			return mcpCompilation{}, translator.NewValidationError("Hermes does not support RemoteMCPServer %q TLS settings", server.Name)
		case server.Spec.Timeout != nil && server.Spec.Timeout.Duration != 30*time.Second:
			return mcpCompilation{}, translator.NewValidationError("Hermes does not support RemoteMCPServer %q timeout", server.Name)
		case tool.Binding.RequireApproval:
			// Hermes denies every approval, so the tool could never run.
			return mcpCompilation{}, translator.NewValidationError("Hermes does not support requireApproval on RemoteMCPServer %q", server.Name)
		case len(tool.Binding.Tools) == 0:
			return mcpCompilation{}, translator.NewValidationError("Hermes binding to RemoteMCPServer %q must list its tools", server.Name)
		case strings.Contains(server.Spec.URL, "${") || slices.ContainsFunc(tool.Binding.Tools, func(t string) bool { return strings.Contains(t, "${") }):
			// Hermes expands ${VAR} in every string of a server entry.
			return mcpCompilation{}, translator.NewValidationError("Hermes RemoteMCPServer %q URL and tool names must not contain ${", server.Name)
		}
		parsed, err := url.Parse(strings.TrimSpace(server.Spec.URL))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
			return mcpCompilation{}, translator.NewValidationError("Hermes RemoteMCPServer %q URL must be an absolute HTTP(S) URL without credentials or fragment", server.Name)
		}
		headers, err := c.compileMCPHeaders(server.Namespace, server.Spec.HeadersFrom)
		if err != nil {
			return mcpCompilation{}, fmt.Errorf("compile RemoteMCPServer %q headers: %w", server.Name, err)
		}
		if server.Spec.ActorJWT != nil {
			headers = translator.MCPActorJWTHeaders(headers)
		}
		selected := slices.Clone(tool.Binding.Tools)
		slices.Sort(selected)
		result.servers[name] = hermesconfig.MCPServer{URL: server.Spec.URL, Headers: headers, Tools: slices.Compact(selected)}
		result.names[server.Namespace+"/"+server.Name] = name
		result.egress = append(result.egress, egress.Origin(parsed))
	}
	return result, nil
}

// compileMCPHeaders resolves literal and ConfigMap headers and writes the
// placeholder for Secret headers. No value may name a variable: Hermes
// expands ${VAR} in MCP headers.
func (c *Compiler) compileMCPHeaders(namespace string, refs []v1alpha3.ValueRef) (map[string]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	headers := make(map[string]string, len(refs))
	for _, ref := range refs {
		if strings.TrimSpace(ref.Name) == "" {
			return nil, translator.NewValidationError("MCP header name is required")
		}
		if _, ok := headers[ref.Name]; ok {
			return nil, translator.NewValidationError("duplicate MCP header %q", ref.Name)
		}
		var value string
		switch {
		case ref.ValueFrom == nil:
			value = ref.Value
		case ref.ValueFrom.Type == v1alpha3.ConfigMapValueSource:
			configMap := krt.FetchOne(c.ctx, c.collections.ConfigMaps, krt.FilterObjectName(types.NamespacedName{Namespace: namespace, Name: ref.ValueFrom.Name}))
			if configMap == nil {
				return nil, fmt.Errorf("ConfigMap %q not found", ref.ValueFrom.Name)
			}
			v, ok := (*configMap).Data[ref.ValueFrom.Key]
			if !ok {
				return nil, fmt.Errorf("ConfigMap %q does not contain key %q", ref.ValueFrom.Name, ref.ValueFrom.Key)
			}
			value = v
		case ref.ValueFrom.Type == v1alpha3.SecretValueSource:
			if err := translator.ValidateSecretKey(c.ctx, c.collections, namespace, ref.ValueFrom.Name, ref.ValueFrom.Key); err != nil {
				return nil, err
			}
			value = translator.CredentialPlaceholder
		default:
			return nil, translator.NewValidationError("unsupported MCP header value source %q", ref.ValueFrom.Type)
		}
		if strings.Contains(value, "${") {
			return nil, translator.NewValidationError("MCP header %q must not contain ${", ref.Name)
		}
		headers[ref.Name] = value
	}
	return headers, nil
}

// compileServerTools maps spec.hermes.serverTools. Only the Anthropic
// Messages API runs them.
func compileServerTools(tools *v1alpha3.HermesServerTools, provider v1alpha3.ModelProvider) (*hermesconfig.ServerTools, error) {
	if tools == nil {
		return nil, nil
	}
	if provider != v1alpha3.ModelProviderAnthropic {
		return nil, translator.NewValidationError("Hermes serverTools require a ModelConfig with the Anthropic provider")
	}
	result := &hermesconfig.ServerTools{CodeExecution: tools.CodeExecution}
	if tools.WebSearch != nil {
		result.WebSearchMaxUses = int(tools.WebSearch.MaxUses)
	}
	if tools.WebFetch != nil {
		result.WebFetchMaxUses = int(tools.WebFetch.MaxUses)
	}
	if result.WebSearchMaxUses == 0 && result.WebFetchMaxUses == 0 && !result.CodeExecution {
		return nil, translator.NewValidationError("Hermes serverTools must select a tool")
	}
	return result, nil
}

// compileMemory maps spec.hermes.memory to the Hermes name of its bound
// RemoteMCPServer. The adapter calls memory_read and memory_write through the
// binding's URL and headers, so the binding's egress and credential cover
// them; the binding must not give those tools to the model.
func compileMemory(memory *v1alpha3.HermesMemory, namespace string, mcp mcpCompilation) (string, error) {
	if memory == nil {
		return "", nil
	}
	name, ok := mcp.names[namespace+"/"+memory.RemoteMCPServer]
	if !ok {
		return "", translator.NewValidationError("Hermes memory RemoteMCPServer %q is not bound to the agent", memory.RemoteMCPServer)
	}
	tools := mcp.servers[name].Tools
	if slices.Contains(tools, hermesconfig.MemoryReadTool) || slices.Contains(tools, hermesconfig.MemoryWriteTool) {
		return "", translator.NewValidationError("Hermes memory RemoteMCPServer %q binding must not list %s or %s", memory.RemoteMCPServer, hermesconfig.MemoryReadTool, hermesconfig.MemoryWriteTool)
	}
	return name, nil
}
