// SPDX-License-Identifier: Apache-2.0

package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	AuthModeAPIKey           = "apiKey"
	AuthModeWorkloadIdentity = "workloadIdentity"
	maxCredentialBytes       = 64 * 1024
	maxTokenResponseBytes    = 128 * 1024
)

// OIDCConfig exchanges a projected Kubernetes ServiceAccount token for a Compute
// access token using an existing Compute OIDC trust. Its client ID is public;
// this grant has no client secret. Audience and issuer are enforced by the trust.
type OIDCConfig struct {
	TokenURL         string
	ClientID         string
	SubjectTokenFile string
}

// ValidateAuth validates configuration without reading files or making requests.
// An empty AuthMode preserves the original API-key configuration.
func (c Config) ValidateAuth() error {
	switch c.AuthMode {
	case "", AuthModeAPIKey:
		if c.TokenFile == "" {
			return errors.New("COMPUTE_TOKEN_FILE is required for apiKey authentication")
		}
		if c.OIDC != (OIDCConfig{}) {
			return errors.New("OIDC settings cannot be combined with apiKey authentication")
		}
	case AuthModeWorkloadIdentity:
		if c.TokenFile != "" {
			return errors.New("COMPUTE_TOKEN_FILE cannot be combined with workloadIdentity authentication")
		}
		if _, err := secureURL(c.OIDC.TokenURL); err != nil {
			return errors.New("COMPUTE_OIDC_TOKEN_URL must be an explicit HTTPS URL without credentials, query, or fragment")
		}
		if !validToken(c.OIDC.ClientID) || len(c.OIDC.ClientID) > 512 || c.OIDC.SubjectTokenFile == "" {
			return errors.New("COMPUTE_OIDC_CLIENT_ID and COMPUTE_OIDC_SUBJECT_TOKEN_FILE are required for workloadIdentity authentication")
		}
	default:
		return errors.New("COMPUTE_AUTH_MODE must be apiKey or workloadIdentity")
	}
	return nil
}

type credentialSource interface {
	bearer(context.Context) (string, error)
}

type fileCredential struct{ path string }

func (s fileCredential) bearer(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.New("Compute credential resolution canceled")
	}
	// Reread a mounted Secret for every request to observe atomic file rotation.
	return readCredential(s.path)
}

type exchangedCredential struct {
	config  OIDCConfig
	http    *http.Client
	timeout time.Duration
	now     func() time.Time

	mu        sync.Mutex
	token     string
	refreshAt time.Time
	refresh   *tokenRefresh
}

type tokenRefresh struct {
	done  chan struct{}
	token string
	err   error
}

func newCredentialSource(c Config, client *http.Client) (credentialSource, error) {
	if c.AuthMode != AuthModeWorkloadIdentity {
		if _, err := readCredential(c.TokenFile); err != nil {
			return nil, err
		}
		return fileCredential{path: c.TokenFile}, nil
	}
	if _, err := readCredential(c.OIDC.SubjectTokenFile); err != nil {
		return nil, errors.New("cannot read a valid projected OIDC subject token")
	}
	return &exchangedCredential{config: c.OIDC, http: client, timeout: c.Timeout, now: time.Now}, nil
}

func (s *exchangedCredential) bearer(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errors.New("Compute token resolution canceled")
	}
	s.mu.Lock()
	if s.token != "" && s.now().Before(s.refreshAt) {
		token := s.token
		s.mu.Unlock()
		return token, nil
	}
	if refresh := s.refresh; refresh != nil {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", errors.New("Compute token resolution canceled")
		case <-refresh.done:
			return refresh.token, refresh.err
		}
	}
	refresh := &tokenRefresh{done: make(chan struct{})}
	s.refresh = refresh
	s.mu.Unlock()

	token, refreshAt, err := s.exchange(ctx)
	s.mu.Lock()
	if err == nil {
		s.token, s.refreshAt = token, refreshAt
	}
	refresh.token, refresh.err = token, err
	close(refresh.done)
	s.refresh = nil
	s.mu.Unlock()
	return token, err
}

func (s *exchangedCredential) exchange(ctx context.Context) (string, time.Time, error) {
	subject, err := readCredential(s.config.SubjectTokenFile)
	if err != nil {
		return "", time.Time{}, errors.New("cannot read a valid projected OIDC subject token")
	}
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"client_id":          {s.config.ClientID},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
		"subject_token":      {subject},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, errors.New("cannot construct Compute token exchange")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nvsentinel-provider-mistral")
	// Count expiry conservatively from request start. Network latency must not
	// make a cached token appear valid beyond the server's stated lifetime.
	started := s.now()
	response, err := s.http.Do(req)
	if err != nil {
		return "", time.Time{}, errors.New("Compute token exchange failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", time.Time{}, fmt.Errorf("Compute token exchange returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseBytes+1))
	if err != nil || len(body) > maxTokenResponseBytes {
		return "", time.Time{}, errors.New("invalid Compute token response")
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil || !validToken(result.AccessToken) ||
		len(result.AccessToken) > maxCredentialBytes || !strings.EqualFold(result.TokenType, "bearer") ||
		result.ExpiresIn <= 0 || result.ExpiresIn > int64((24*time.Hour)/time.Second) {
		return "", time.Time{}, errors.New("invalid Compute token response")
	}
	lifetime := time.Duration(result.ExpiresIn) * time.Second
	// Refresh early enough for one full reboot HTTP timeout plus clock skew.
	// Reject an already-short token instead of attempting the destructive POST.
	refreshAt := started.Add(lifetime - s.timeout - 30*time.Second)
	if !s.now().Before(refreshAt) {
		return "", time.Time{}, errors.New("Compute access token lifetime is too short for a reboot request")
	}
	return result.AccessToken, refreshAt, nil
}

func secureURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("invalid HTTPS endpoint")
	}
	return u, nil
}

func readCredential(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot read Compute credential file")
	}
	defer file.Close()
	credential, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil || len(credential) > maxCredentialBytes {
		return "", errors.New("cannot read a valid Compute credential file")
	}
	token := strings.TrimSpace(string(credential))
	if !validToken(token) {
		return "", errors.New("Compute credential is empty or contains invalid characters")
	}
	return token, nil
}

func validToken(token string) bool {
	if token == "" {
		return false
	}
	for _, b := range []byte(token) {
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}
