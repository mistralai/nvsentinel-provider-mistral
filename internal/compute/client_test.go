// SPDX-License-Identifier: Apache-2.0

package compute

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRebootContractAndCredentialRotation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/prefix/v1/projects/default/nodes/host-1/reboot" || r.URL.Query().Get("region") != "region-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		want := "Bearer first"
		if n == 2 {
			want = "Bearer rotated"
		}
		if r.Header.Get("Authorization") != want {
			t.Error("credential was not read from the current file")
		}
		if r.ContentLength != 0 {
			t.Error("reboot must have no request body")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	path := tokenFile(t, "first\n")
	client, err := New(Config{Endpoint: server.URL + "/prefix", Project: "default", Region: "region-1", TokenFile: path, Timeout: time.Second}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Reboot(context.Background(), "host-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.Reboot(context.Background(), "host-1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestRedirectAndErrorBodiesAreNotFollowedOrExposed(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte("sensitive response marker"))
	}))
	defer server.Close()
	client, err := New(Config{Endpoint: server.URL, Project: "default", Region: "region-1", TokenFile: tokenFile(t, "secret"), Timeout: time.Second}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.Reboot(context.Background(), "host-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 307 {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "sensitive") || destinationCalls.Load() != 0 {
		t.Fatal("redirect or sensitive error escaped")
	}
}

func TestTimeoutNeverRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := New(Config{Endpoint: server.URL, Project: "default", Region: "region-1", TokenFile: tokenFile(t, "secret"), Timeout: 50 * time.Millisecond}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Reboot(context.Background(), "host-1"); err == nil {
		t.Fatal("timeout must be surfaced")
	}
	if calls.Load() != 1 {
		t.Fatalf("requests = %d; expected one", calls.Load())
	}
}

func TestInvalidInputsNeverReachAPI(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	client, err := New(Config{Endpoint: server.URL, Project: "default", Region: "region-1", TokenFile: tokenFile(t, "secret"), Timeout: time.Second}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"", "../other", "host/other", "host?region=other"} {
		if err := client.Reboot(context.Background(), node); err == nil {
			t.Errorf("accepted %q", node)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached the API")
	}
	for _, endpoint := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com?token=x"} {
		if _, err := New(Config{Endpoint: endpoint, Project: "default", Region: "region-1", TokenFile: "file", Timeout: time.Second}, nil); err == nil {
			t.Errorf("accepted endpoint %s", endpoint)
		}
	}
}
