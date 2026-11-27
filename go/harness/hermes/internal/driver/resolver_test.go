package driver

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// resolverScript resolves the runtime provider the way `hermes gateway` does
// for a turn, and prints it with the key reduced to whether it is ours.
const resolverScript = `
import json, sys
from hermes_cli.runtime_provider import resolve_runtime_with_fallback
from gateway.run import _load_gateway_config
try:
    runtime, fallback = resolve_runtime_with_fallback(_load_gateway_config())
except Exception as exc:
    print(json.dumps({"error": type(exc).__name__ + ": " + str(exc)}))
    sys.exit(0)
print(json.dumps({"provider": runtime.get("provider"), "base_url": runtime.get("base_url"),
                  "api_mode": runtime.get("api_mode"), "api_key": runtime.get("api_key") if isinstance(runtime.get("api_key"), str) else "<callable>",
                  "fallback": fallback is not None}))
`

// hermesPython returns the command that runs Python with Hermes importable,
// from KAGENT_HERMES_IMAGE (a pinned Hermes Agent image, run with docker) or
// KAGENT_HERMES_ROOT (an extracted image root filesystem).
func hermesPython(t *testing.T, home string, environment []string) *exec.Cmd {
	t.Helper()
	if image := os.Getenv("KAGENT_HERMES_IMAGE"); image != "" {
		user := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
		args := []string{"run", "--rm", "--network", "none", "--user", user, "--entrypoint", "/opt/hermes/.venv/bin/python", "-v", home + ":" + home, "-w", home}
		for _, entry := range environment {
			if !strings.HasPrefix(entry, "PATH=") {
				args = append(args, "-e", entry)
			}
		}
		return exec.Command("docker", append(args, image, "-s", "-P", "-c", resolverScript)...)
	}
	root := os.Getenv("KAGENT_HERMES_ROOT")
	if root == "" {
		t.Skip("set KAGENT_HERMES_IMAGE or KAGENT_HERMES_ROOT to run the real Hermes resolver")
	}
	hermes := filepath.Join(root, "opt", "hermes")
	pythons, _ := filepath.Glob(filepath.Join(hermes, "tools", "python-*", "bin", "python3"))
	require.NotEmpty(t, pythons, "no Python under %s/tools", hermes)
	sitePackages, _ := filepath.Glob(filepath.Join(hermes, ".venv", "lib", "python3.*", "site-packages"))
	require.NotEmpty(t, sitePackages)
	cmd := exec.Command(pythons[0], "-s", "-P", "-c", resolverScript)
	cmd.Dir = home
	cmd.Env = append(environment, "PYTHONPATH="+sitePackages[0]+string(os.PathListSeparator)+hermes)
	return cmd
}

// The config the adapter writes must make Hermes 0.23.4 resolve the compiled
// endpoint with the placeholder key, for a remote and a loopback host, and
// never fall back to a provider key in the environment.
func TestHermesResolvesTheCompiledEndpoint(t *testing.T) {
	for _, baseURL := range []string{"https://openrouter.ai/api/v1", "http://127.0.0.1:9999/v1", "https://llm.example.com/v1"} {
		t.Run(baseURL, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, WriteHome(home, Settings{Model: "deepseek/deepseek-v4.1-flash-1017", BaseURL: baseURL}))
			// Decoy keys that the allowlist drops; the second set reaches
			// Hermes directly, to prove the entry's key wins regardless.
			environment := gatewayEnvironment([]string{"PATH=/usr/bin:/bin", "HOME=" + home, "OPENROUTER_API_KEY=sk-or-v1-decoy-allowlisted-away"}, home, 8642, strings.Repeat("k", 32))
			environment = append(environment, "OPENROUTER_API_KEY=sk-or-v1-decoydecoydecoy", "OPENAI_API_KEY=decoy-openai-key-value")
			out, err := hermesPython(t, home, environment).CombinedOutput()
			require.NoError(t, err, string(out))
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			var runtime map[string]any
			require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &runtime), string(out))
			require.Equal(t, map[string]any{
				"provider": "custom", "base_url": baseURL, "api_mode": "chat_completions",
				"api_key": CredentialPlaceholder, "fallback": false,
			}, runtime, string(out))
		})
	}
}
