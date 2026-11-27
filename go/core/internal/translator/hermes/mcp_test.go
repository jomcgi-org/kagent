package hermes

import (
	"context"
	"strings"
	"testing"

	"istio.io/istio/pkg/kube/krt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	hermesconfig "github.com/kagent-dev/kagent/go/harness/hermes/config"
)

func mcpServer(name, url string, headers ...v1alpha3.ValueRef) *v1alpha3.RemoteMCPServer {
	return &v1alpha3.RemoteMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test", UID: types.UID("mcp-" + name)},
		Spec:       v1alpha3.RemoteMCPServerSpec{URL: url, Protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp, HeadersFrom: headers},
	}
}

// bindMCP adds the server to the template and to the resolved input, as the
// translator's resolution does.
func bindMCP(input *translator.HarnessInput, server *v1alpha3.RemoteMCPServer, tools ...string) {
	binding := v1alpha3.MCPToolBinding{Server: corev1.TypedObjectReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: tools}
	input.Root.Template.Spec.Tools = append(input.Root.Template.Spec.Tools, v1alpha3.ToolBinding{MCP: &binding})
	input.Root.MCPTools = append(input.Root.MCPTools, translator.ResolvedMCPTool{Binding: binding, Server: server})
}

func TestCompileMCPServers(t *testing.T) {
	input, collections := testInput(t, openRouter(v1alpha3.ModelConfigSpec{}), map[string][]byte{"token": []byte(credentialValue)})
	recall := mcpServer("recall", "http://hindsight.test.svc:8888/mcp/", v1alpha3.ValueRef{Name: "Authorization", ValueFrom: &v1alpha3.ValueSource{Type: v1alpha3.SecretValueSource, Name: "model-auth", Key: "token"}})
	public := mcpServer("scotland", "https://scotland.example.com/mcp")
	bindMCP(input, recall, "recall")
	bindMCP(input, public, "plan", "check", "plan")

	result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	revision := result.Revision
	if strings.Contains(string(revision.ConfigJSON), credentialValue) {
		t.Fatal("config contains the MCP credential")
	}
	cfg, err := hermesconfig.Parse(revision.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	memory := cfg.MCPServers["recall"]
	if memory.URL != recall.Spec.URL || memory.Headers["Authorization"] != translator.CredentialPlaceholder || strings.Join(memory.Tools, ",") != "recall" {
		t.Fatalf("memory server = %#v", memory)
	}
	if got := strings.Join(cfg.MCPServers["scotland"].Tools, ","); got != "check,plan" || len(cfg.MCPServers["scotland"].Headers) != 0 {
		t.Fatalf("scotland server = %#v", cfg.MCPServers["scotland"])
	}
	if len(revision.Credentials) != 1 || revision.Credentials[0].Origin != "http://hindsight.test.svc:8888" || revision.Credentials[0].Header != "authorization" {
		t.Fatalf("credentials = %#v", revision.Credentials)
	}
	want := "http://hindsight.test.svc:8888,http://kagent-controller.kagent:8083,https://openrouter.ai:443,https://scotland.example.com:443"
	if got := strings.Join(revision.EgressDestinations, ","); got != want {
		t.Fatalf("egress = %s, want %s", got, want)
	}
	if !strings.Contains(string(revision.Provenance), "RemoteMCPServer") {
		t.Fatalf("provenance lacks the MCP servers: %s", revision.Provenance)
	}
}

func TestCompileMCPRejectsUnsafeBindings(t *testing.T) {
	passthrough := true
	tests := []struct {
		name    string
		server  *v1alpha3.RemoteMCPServer
		tools   []string
		approve bool
		wantErr string
	}{
		{name: "all tools", server: mcpServer("open", "https://mcp.example.com/mcp"), wantErr: "must list its tools"},
		{name: "approval", server: mcpServer("open", "https://mcp.example.com/mcp"), tools: []string{"read"}, approve: true, wantErr: "requireApproval"},
		{name: "passthrough", server: func() *v1alpha3.RemoteMCPServer {
			s := mcpServer("open", "https://mcp.example.com/mcp")
			s.Spec.TokenPassthrough = &passthrough
			return s
		}(), tools: []string{"read"}, wantErr: "tokenPassthrough"},
		{name: "SSE", server: func() *v1alpha3.RemoteMCPServer {
			s := mcpServer("open", "https://mcp.example.com/mcp")
			s.Spec.Protocol = v1alpha3.RemoteMCPServerProtocolSse
			return s
		}(), tools: []string{"read"}, wantErr: "Streamable HTTP"},
		{name: "variable header", server: mcpServer("open", "https://mcp.example.com/mcp", v1alpha3.ValueRef{Name: "X-Key", Value: "${HOME}"}), tools: []string{"read"}, wantErr: "must not contain ${"},
		{name: "variable URL", server: mcpServer("open", "https://mcp.example.com/mcp?k=${API_SERVER_KEY}"), tools: []string{"read"}, wantErr: "must not contain ${"},
		{name: "variable tool", server: mcpServer("open", "https://mcp.example.com/mcp"), tools: []string{"${TOOL}"}, wantErr: "must not contain ${"},
		{name: "toolset name", server: mcpServer("memory", "https://mcp.example.com/mcp"), tools: []string{"read"}, wantErr: "Hermes toolset name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, collections := testInput(t, openRouter(v1alpha3.ModelConfigSpec{}), nil)
			bindMCP(input, test.server, test.tools...)
			input.Root.MCPTools[0].Binding.RequireApproval = test.approve
			_, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Compile() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestCompileServerTools(t *testing.T) {
	tools, err := compileServerTools(&v1alpha3.HermesServerTools{
		WebSearch: &v1alpha3.HermesServerToolLimit{MaxUses: 5}, WebFetch: &v1alpha3.HermesServerToolLimit{MaxUses: 3}, CodeExecution: true,
	}, v1alpha3.ModelProviderAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if *tools != (hermesconfig.ServerTools{WebSearchMaxUses: 5, WebFetchMaxUses: 3, CodeExecution: true}) {
		t.Fatalf("server tools = %#v", tools)
	}
	if _, err := compileServerTools(&v1alpha3.HermesServerTools{}, v1alpha3.ModelProviderAnthropic); err == nil || !strings.Contains(err.Error(), "select a tool") {
		t.Fatalf("empty serverTools error = %v", err)
	}
	if tools, err := compileServerTools(nil, v1alpha3.ModelProviderOpenAI); tools != nil || err != nil {
		t.Fatalf("unset serverTools = %#v, %v", tools, err)
	}
}

func TestCompileAnthropicWithServerToolsAndMCP(t *testing.T) {
	input, collections := testInput(t, v1alpha3.ModelConfigSpec{
		Provider: v1alpha3.ModelProviderAnthropic, Model: "claude-opus-5-5", APIKeySecret: "model-auth", APIKeySecretKey: "api-key",
	}, map[string][]byte{"api-key": []byte(credentialValue)})
	input.Harness.Spec.Hermes.ServerTools = &v1alpha3.HermesServerTools{WebSearch: &v1alpha3.HermesServerToolLimit{MaxUses: 5}, CodeExecution: true}
	bindMCP(input, mcpServer("scotland", "https://scotland.example.com/mcp"), "plan")
	result, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := hermesconfig.Parse(result.Revision.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerTools == nil || *cfg.ServerTools != (hermesconfig.ServerTools{WebSearchMaxUses: 5, CodeExecution: true}) {
		t.Fatalf("server tools = %#v", cfg.ServerTools)
	}
	// Server tools run at Anthropic: the only new origin is the MCP server's.
	want := "http://kagent-controller.kagent:8083,https://api.anthropic.com:443,https://scotland.example.com:443"
	if got := strings.Join(result.Revision.EgressDestinations, ","); got != want {
		t.Fatalf("egress = %s, want %s", got, want)
	}
}

func TestCompileMCPRejectsServersWithOneHermesName(t *testing.T) {
	input, collections := testInput(t, openRouter(v1alpha3.ModelConfigSpec{}), nil)
	bindMCP(input, mcpServer("foo-bar", "https://a.example.com/mcp"), "lookup")
	bindMCP(input, mcpServer("foo.bar", "https://b.example.com/mcp"), "lookup")
	_, err := NewCompiler(krt.TestingDummyContext{}, collections, testRuntimeDefaults()).Compile(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "same Hermes tool prefix") {
		t.Fatalf("Compile() error = %v", err)
	}
}
