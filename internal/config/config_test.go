// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
	"time"

	"github.com/mistralai/nvsentinel-provider-mistral/internal/compute"
)

func TestConfigurationBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value string
	}{
		{"namespace", "POD_NAMESPACE", ""},
		{"tls cert", "TLS_CERT_FILE", ""},
		{"tls key", "TLS_KEY_FILE", ""},
		{"wrong identity", "AUTH_SERVICE_ACCOUNT", "janitor"},
		{"unbounded timeout", "RPC_TIMEOUT", "0"},
		{"excessive timeout", "RPC_TIMEOUT", "6m"},
		{"short RPC", "RPC_TIMEOUT", "10s"},
		{"invalid HTTP timeout", "HTTP_TIMEOUT", "forever"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{"POD_NAMESPACE": "nvsentinel", "TLS_CERT_FILE": "cert", "TLS_KEY_FILE": "key", "COMPUTE_TOKEN_FILE": "token"}
			values[tc.key] = tc.value
			if _, err := FromEnv(func(key string) string { return values[key] }); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	values := map[string]string{"POD_NAMESPACE": "nvsentinel", "TLS_CERT_FILE": "cert", "TLS_KEY_FILE": "key", "COMPUTE_TOKEN_FILE": "token"}
	cfg, err := FromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RPCTimeout != 45*time.Second || cfg.Compute.Timeout != 30*time.Second || cfg.ServiceAccount != "system:serviceaccount:nvsentinel:janitor" {
		t.Fatal("unexpected defaults")
	}
	if cfg.NodeNameLabel != "nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name" {
		t.Fatalf("default node-name label is not protected: %q", cfg.NodeNameLabel)
	}
}

func TestWorkloadIdentityEnvironment(t *testing.T) {
	values := map[string]string{
		"POD_NAMESPACE": "nvsentinel", "TLS_CERT_FILE": "cert", "TLS_KEY_FILE": "key",
		"COMPUTE_AUTH_MODE": "workloadIdentity", "COMPUTE_ENDPOINT": "https://api.compute.example",
		"COMPUTE_PROJECT": "default", "COMPUTE_REGION": "region-1",
		"COMPUTE_OIDC_TOKEN_URL": "https://auth.compute.example/oauth2/token",
		"COMPUTE_OIDC_CLIENT_ID": "trust-client", "COMPUTE_OIDC_SUBJECT_TOKEN_FILE": "/projected/token",
	}
	cfg, err := FromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Compute.AuthMode != compute.AuthModeWorkloadIdentity || cfg.Compute.OIDC.ClientID != "trust-client" ||
		cfg.Compute.OIDC.TokenURL != values["COMPUTE_OIDC_TOKEN_URL"] || cfg.Compute.OIDC.SubjectTokenFile != "/projected/token" ||
		cfg.Compute.TokenFile != "" || cfg.RPCTimeout != 75*time.Second {
		t.Fatal("workload identity configuration did not preserve the explicit trust settings and timeout")
	}
	for _, key := range []string{"COMPUTE_OIDC_TOKEN_URL", "COMPUTE_OIDC_CLIENT_ID", "COMPUTE_OIDC_SUBJECT_TOKEN_FILE"} {
		saved := values[key]
		delete(values, key)
		if _, err := FromEnv(func(key string) string { return values[key] }); err == nil {
			t.Errorf("missing %s was accepted", key)
		}
		values[key] = saved
	}
	values["COMPUTE_TOKEN_FILE"] = "static-token"
	if _, err := FromEnv(func(key string) string { return values[key] }); err == nil {
		t.Error("mixed authentication configuration was accepted")
	}
	delete(values, "COMPUTE_TOKEN_FILE")
	values["RPC_TIMEOUT"] = "60s"
	if _, err := FromEnv(func(key string) string { return values[key] }); err == nil {
		t.Error("RPC timeout did not reserve time for both token exchange and reboot")
	}
}
