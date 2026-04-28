package driver

import "testing"

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, name := range proxyEnvNames {
		t.Setenv(name, "")
	}
}

func TestProxyEnvFromEnvironment(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
	t.Setenv("NO_PROXY", "localhost,.svc")

	got := ProxyEnvFromEnvironment()
	env := map[string]string{}
	for _, item := range got {
		env[item.Name] = item.Value
	}

	if env["HTTPS_PROXY"] != "http://proxy.example:8080" {
		t.Fatalf("HTTPS_PROXY = %q", env["HTTPS_PROXY"])
	}
	if env["NO_PROXY"] != "localhost,.svc" {
		t.Fatalf("NO_PROXY = %q", env["NO_PROXY"])
	}
	if _, ok := env["HTTP_PROXY"]; ok {
		t.Fatal("empty HTTP_PROXY should not be returned")
	}
}
