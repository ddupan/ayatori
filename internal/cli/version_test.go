package cli

import (
	"strings"
	"testing"
)

// release 是测试里环境发布的版本。
const release = "v0.6.0"

func TestSkewWarning(t *testing.T) {
	for _, c := range []struct {
		cli, server string
		warn        bool
	}{
		{release, "v0.6.3", false},
		{"v0.7.0", release, false},
		{"v0.5.2", release, false},
		{"v0.8.0", release, true},
		{"v0.4.9", release, true},
		{"v1.0.0", "v0.9.0", true},
		{"v0.7.0-rc.1", release, false},
		{"dev", release, false},
		{release, "", false},
	} {
		if got := SkewWarning(c.cli, c.server) != ""; got != c.warn {
			t.Errorf("SkewWarning(%q, %q) warns = %v, want %v", c.cli, c.server, got, c.warn)
		}
	}
}

func TestClientGoVersion(t *testing.T) {
	if v := ClientGoVersion(); !strings.HasPrefix(v, "v0.") {
		t.Errorf("client-go version %q", v)
	}
}
