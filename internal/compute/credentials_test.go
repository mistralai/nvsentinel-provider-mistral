// SPDX-License-Identifier: Apache-2.0

package compute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func workloadConfig(server *httptest.Server, subjectFile string) Config {
	return Config{
		Endpoint: server.URL, Project: "default", Region: "region-1", Timeout: time.Second,
		AuthMode: AuthModeWorkloadIdentity,
		OIDC:     OIDCConfig{TokenURL: server.URL + "/oauth2/token", ClientID: "test-trust-client", SubjectTokenFile: subjectFile},
	}
}

func TestWorkloadIdentityExchangeCacheAndRotation(t *testing.T) {
	var exchanges, reboots atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			request := exchanges.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
				r.Header.Get("Authorization") != "" || len(r.Form) != 4 || r.Form.Get("client_id") != "test-trust-client" ||
				r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" ||
				r.Form.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:jwt" {
				t.Error("incorrect federated token exchange contract")
			}
			if r.Form.Get("subject_token") != fmt.Sprintf("projected-token-%d", request) {
				t.Error("exchange did not read the latest projected token")
			}
			_, _ = fmt.Fprintf(w, `{"access_token":"access-token-%d","token_type":"bearer","expires_in":3600}`, request)
			return
		}
		reboots.Add(1)
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer access-token-%d", exchanges.Load()) {
			t.Error("reboot did not use the exchanged access token")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	file := tokenFile(t, "projected-token-1\n")
	client, err := New(workloadConfig(server, file), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 0 {
		t.Fatal("client construction made a network request")
	}
	clock := time.Unix(1_700_000_000, 0)
	client.tokens.(*exchangedCredential).now = func() time.Time { return clock }
	if err := client.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 1 || reboots.Load() != 0 {
		t.Fatal("credential preflight must only exchange the token")
	}
	if err := client.Reboot(context.Background(), "host-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("projected-token-2"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.Reboot(context.Background(), "host-2"); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 1 {
		t.Fatal("valid access token was not cached")
	}
	clock = clock.Add(3570 * time.Second)
	if err := client.Reboot(context.Background(), "host-3"); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 2 || reboots.Load() != 3 {
		t.Fatalf("exchanges=%d reboots=%d; wanted rotation and one POST per requested reboot", exchanges.Load(), reboots.Load())
	}
}

func TestConcurrentCredentialPreflightSharesOneExchange(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"access-token","token_type":"bearer","expires_in":3600}`))
	}))
	defer server.Close()
	client, err := New(workloadConfig(server, tokenFile(t, "projected-token")), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	const concurrent = 24
	errors := make([]error, concurrent)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errors[i] = client.Prepare(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatalf("concurrent preflight made %d exchanges; want one", exchanges.Load())
	}
}

func TestCanceledCredentialWaiterDoesNotBlockOnAnotherExchange(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"access_token":"access-token","token_type":"bearer","expires_in":3600}`))
	}))
	defer server.Close()
	client, err := New(workloadConfig(server, tokenFile(t, "projected-token")), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- client.Prepare(context.Background()) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.Prepare(ctx); err == nil {
		t.Error("canceled waiter unexpectedly received a token")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestFailedRefreshDoesNotUseAgingToken(t *testing.T) {
	var exchanges, reboots atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			if exchanges.Add(1) == 2 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("private-token-exchange-response"))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"access-token","token_type":"bearer","expires_in":3600}`))
			return
		}
		reboots.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := New(workloadConfig(server, tokenFile(t, "projected-token")), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_700_000_000, 0)
	client.tokens.(*exchangedCredential).now = func() time.Time { return clock }
	if err := client.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(3570 * time.Second)
	err = client.Reboot(context.Background(), "host-1")
	if err == nil || strings.Contains(err.Error(), "private") || reboots.Load() != 0 || exchanges.Load() != 2 {
		t.Fatalf("refresh failure must prevent reboot and hide body: %v exchanges=%d reboots=%d", err, exchanges.Load(), reboots.Load())
	}
	if err := client.Prepare(context.Background()); err != nil {
		t.Fatalf("a later preflight must be able to recover: %v", err)
	}
}

func TestInvalidTokenResponsesNeverReachReboot(t *testing.T) {
	tests := map[string]string{
		"missing expiry":        `{"access_token":"secret","token_type":"bearer"}`,
		"negative expiry":       `{"access_token":"secret","token_type":"bearer","expires_in":-1}`,
		"overflow expiry":       `{"access_token":"secret","token_type":"bearer","expires_in":9223372036854775807}`,
		"insufficient lifetime": `{"access_token":"secret","token_type":"bearer","expires_in":10}`,
		"wrong type":            `{"access_token":"secret","token_type":"mac","expires_in":3600}`,
		"missing token":         `{"token_type":"bearer","expires_in":3600}`,
		"invalid token":         `{"access_token":"secret\nheader","token_type":"bearer","expires_in":3600}`,
		"malformed JSON":        `secret-invalid-json`,
		"trailing JSON":         `{"access_token":"secret","token_type":"bearer","expires_in":3600} {}`,
		"oversized response":    strings.Repeat("secret", maxTokenResponseBytes),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			var exchanges, reboots atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth2/token" {
					exchanges.Add(1)
					_, _ = w.Write([]byte(body))
					return
				}
				reboots.Add(1)
			}))
			defer server.Close()
			client, err := New(workloadConfig(server, tokenFile(t, "projected-token")), server.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = client.Reboot(context.Background(), "host-1")
			if err == nil || strings.Contains(err.Error(), "secret") || exchanges.Load() != 1 || reboots.Load() != 0 {
				t.Fatalf("invalid response allowed reboot or leaked detail: %v exchanges=%d reboots=%d", err, exchanges.Load(), reboots.Load())
			}
		})
	}
}

func TestRejectedRebootDoesNotRefreshOrReplay(t *testing.T) {
	var exchanges, reboots atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			exchanges.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"access-token","token_type":"bearer","expires_in":3600}`))
			return
		}
		reboots.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client, err := New(workloadConfig(server, tokenFile(t, "projected-token")), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.Reboot(context.Background(), "host-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || exchanges.Load() != 1 || reboots.Load() != 1 {
		t.Fatalf("rejected reboot retried or refreshed: %v exchanges=%d reboots=%d", err, exchanges.Load(), reboots.Load())
	}
}

func TestTokenExchangeRedirectNeverForwardsIdentity(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte("private-redirect-body"))
	}))
	defer server.Close()
	client, err := New(workloadConfig(server, tokenFile(t, "projected-token")), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Prepare(context.Background()); err == nil || strings.Contains(err.Error(), "private") || destinationCalls.Load() != 0 {
		t.Fatalf("redirect leaked the subject token or body: %v destination=%d", err, destinationCalls.Load())
	}
}

func TestTokenExchangeTimeoutNeverReachesReboot(t *testing.T) {
	var exchanges, reboots atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			exchanges.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		reboots.Add(1)
	}))
	defer server.Close()
	cfg := workloadConfig(server, tokenFile(t, "projected-token"))
	cfg.Timeout = 50 * time.Millisecond
	client, err := New(cfg, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.Reboot(context.Background(), "host-1")
	if err == nil || strings.Contains(err.Error(), server.URL) || exchanges.Load() != 1 || reboots.Load() != 0 {
		t.Fatalf("timed-out exchange was retried or allowed reboot: %v exchanges=%d reboots=%d", err, exchanges.Load(), reboots.Load())
	}
}

func TestAuthenticationConfigurationRejectsMixedModes(t *testing.T) {
	validOIDC := OIDCConfig{TokenURL: "https://compute.example/oauth2/token", ClientID: "trust-client", SubjectTokenFile: "/projected/token"}
	tests := []Config{
		{AuthMode: "unknown", TokenFile: "file"},
		{AuthMode: AuthModeAPIKey},
		{AuthMode: AuthModeAPIKey, TokenFile: "file", OIDC: validOIDC},
		{AuthMode: AuthModeWorkloadIdentity, TokenFile: "file", OIDC: validOIDC},
		{AuthMode: AuthModeWorkloadIdentity},
	}
	for _, rawURL := range []string{"", "http://compute.example/token", "https://user:password@compute.example/token", "https://compute.example/token?x=1", "https://compute.example/token#secret"} {
		cfg := Config{AuthMode: AuthModeWorkloadIdentity, OIDC: validOIDC}
		cfg.OIDC.TokenURL = rawURL
		tests = append(tests, cfg)
	}
	for _, cfg := range tests {
		if err := cfg.ValidateAuth(); err == nil || strings.Contains(err.Error(), "password") {
			t.Fatal("mixed, incomplete, or unsafe authentication accepted or configuration leaked")
		}
	}
	for _, cfg := range []Config{{TokenFile: "file"}, {AuthMode: AuthModeAPIKey, TokenFile: "file"}, {AuthMode: AuthModeWorkloadIdentity, OIDC: validOIDC}} {
		if err := cfg.ValidateAuth(); err != nil {
			t.Fatalf("valid authentication configuration rejected: %v", err)
		}
	}
}
