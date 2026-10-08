// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mistralai/nvsentinel-provider-mistral/internal/compute"
	"github.com/mistralai/nvsentinel-provider-mistral/internal/provider"
)

type Config struct {
	ListenAddress, HealthAddress string
	Namespace, NodeNameLabel     string
	TLSCertFile, TLSKeyFile      string
	Audience, ServiceAccount     string
	RPCTimeout                   time.Duration
	Compute                      compute.Config
}

func Load() (Config, error) { return FromEnv(os.Getenv) }

func FromEnv(get func(string) string) (Config, error) {
	value := func(key, fallback string) string {
		if v := get(key); v != "" {
			return v
		}
		return fallback
	}
	c := Config{
		ListenAddress:  value("LISTEN_ADDRESS", ":50051"),
		HealthAddress:  value("HEALTH_ADDRESS", ":8080"),
		Namespace:      get("POD_NAMESPACE"),
		NodeNameLabel:  value("NODE_NAME_LABEL", provider.DefaultNodeNameLabel),
		TLSCertFile:    get("TLS_CERT_FILE"),
		TLSKeyFile:     get("TLS_KEY_FILE"),
		Audience:       value("AUTH_AUDIENCE", "nvsentinel-csp-provider"),
		ServiceAccount: value("AUTH_SERVICE_ACCOUNT", "system:serviceaccount:nvsentinel:janitor"),
		Compute: compute.Config{
			Endpoint: get("COMPUTE_ENDPOINT"), Project: get("COMPUTE_PROJECT"),
			Region: get("COMPUTE_REGION"), TokenFile: get("COMPUTE_TOKEN_FILE"),
		},
	}
	if c.Namespace == "" || c.TLSCertFile == "" || c.TLSKeyFile == "" {
		return Config{}, errors.New("POD_NAMESPACE, TLS_CERT_FILE, and TLS_KEY_FILE are required")
	}
	parts := strings.Split(c.ServiceAccount, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" || parts[3] == "" {
		return Config{}, errors.New("AUTH_SERVICE_ACCOUNT must be an exact Kubernetes ServiceAccount subject")
	}
	var err error
	c.Compute.Timeout, err = duration(value("HTTP_TIMEOUT", "30s"), "HTTP_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	c.RPCTimeout, err = duration(value("RPC_TIMEOUT", "45s"), "RPC_TIMEOUT")
	if err != nil {
		return Config{}, err
	}
	if c.RPCTimeout <= c.Compute.Timeout {
		return Config{}, errors.New("RPC_TIMEOUT must exceed HTTP_TIMEOUT to allow durable bookkeeping")
	}
	return c, nil
}

func duration(value, name string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 || d > 5*time.Minute {
		return 0, fmt.Errorf("%s must be a positive duration no greater than 5m", name)
	}
	return d, nil
}
