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
