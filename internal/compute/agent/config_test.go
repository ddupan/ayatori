package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.ddupan.top/ayatori/internal/compute/agent"
)

func TestLoadConfigRejectsInvalidNodeLabels(t *testing.T) {
	for name, labels := range map[string]string{
		"role label":  "node-role.kubernetes.io/compute: \"\"",
		"invalid key": "\"not a/valid/key\": x",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			config := "nodeName: n\nkubeconfig: k\nstateDir: s\nnodeLabels:\n  " + labels + "\n"
			if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := agent.LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), "nodeLabels") {
				t.Fatalf("want a nodeLabels error, got %v", err)
			}
		})
	}
}

func TestLoadConfigConsole(t *testing.T) {
	load := func(console string) (agent.Config, error) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("nodeName: n\nkubeconfig: k\nstateDir: s\nconsole:\n"+console), 0o600); err != nil {
			t.Fatal(err)
		}
		return agent.LoadConfig(path)
	}
	c, err := load("  address: :9812\n  proxyID: spiffe://example.org/console-proxy\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Console.WorkloadAPI != "unix:///run/spire/agent-sockets/spire-agent.sock" {
		t.Errorf("workloadAPI default: %q", c.Console.WorkloadAPI)
	}
	for name, console := range map[string]string{
		"no address":       "  proxyID: spiffe://example.org/console-proxy\n",
		"no proxy":         "  address: :9812\n",
		"invalid proxy ID": "  address: :9812\n  proxyID: https://example.org/console-proxy\n",
	} {
		if _, err := load(console); err == nil || !strings.Contains(err.Error(), "console.") {
			t.Errorf("%s: want a console error, got %v", name, err)
		}
	}
}
