package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: east-cluster
  cluster:
    server: https://east.example:6443
    insecure-skip-tls-verify: true
- name: west-cluster
  cluster:
    server: https://west.example:6443
    insecure-skip-tls-verify: true
contexts:
- name: east
  context: {cluster: east-cluster, user: u}
- name: west
  context: {cluster: west-cluster, user: u}
current-context: east
users:
- name: u
  user: {token: not-a-real-token}
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRestConfigSelectsNamedContext(t *testing.T) {
	path := writeKubeconfig(t)
	for ctx, host := range map[string]string{"east": "https://east.example:6443", "west": "https://west.example:6443"} {
		cfg, err := restConfig(path, ctx)
		if err != nil {
			t.Fatalf("restConfig(%q): %v", ctx, err)
		}
		if cfg.Host != host {
			t.Errorf("context %q: Host = %q, want %q", ctx, cfg.Host, host)
		}
	}
}

func TestRestConfigRefusesUnknownContext(t *testing.T) {
	// client-go would silently fall back to current-context; we must not
	_, err := restConfig(writeKubeconfig(t), "north")
	if err == nil || !strings.Contains(err.Error(), `context "north" not found`) {
		t.Errorf("got %v, want a 'context \"north\" not found' error", err)
	}
}

func TestRestConfigUsesKUBECONFIGWhenNoExplicitPath(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t))
	cfg, err := restConfig("", "west")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://west.example:6443" {
		t.Errorf("Host = %q", cfg.Host)
	}
}

func TestNewClientBuildsWithoutContactingTheCluster(t *testing.T) {
	c, err := NewClient(writeKubeconfig(t), "east", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != "east" || c.dyn == nil || c.mapper == nil {
		t.Errorf("client not fully initialised: %+v", c)
	}
	if _, err := NewClient(writeKubeconfig(t), "north", 5*time.Second); err == nil {
		t.Error("unknown context: expected an error")
	}
}
