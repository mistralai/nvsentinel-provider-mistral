// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
	"time"
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
			values := map[string]string{"POD_NAMESPACE": "nvsentinel", "TLS_CERT_FILE": "cert", "TLS_KEY_FILE": "key"}
			values[tc.key] = tc.value
			if _, err := FromEnv(func(key string) string { return values[key] }); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	values := map[string]string{"POD_NAMESPACE": "nvsentinel", "TLS_CERT_FILE": "cert", "TLS_KEY_FILE": "key"}
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
