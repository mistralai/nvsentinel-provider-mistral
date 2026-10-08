// SPDX-License-Identifier: Apache-2.0

// Package compute implements the small part of the Compute API used for rebooting.
package compute

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	projectPattern = regexp.MustCompile(`^[a-z]([a-z0-9]|-[a-z0-9])*$`)
	namePattern    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

type Config struct {
	Endpoint  string
	Project   string
	Region    string
	AuthMode  string
	TokenFile string
	OIDC      OIDCConfig
	Timeout   time.Duration
}

type Client struct {
	config Config
	base   *url.URL
	http   *http.Client
	tokens credentialSource
}

// APIError intentionally excludes response bodies: they may contain sensitive data.
type APIError struct{ StatusCode int }

// Scope binds durable attempts to one API endpoint, project, and region.
func (c *Client) Scope() string {
	base := *c.base
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""
	digest := sha256.Sum256([]byte(base.String() + "\n" + c.config.Project + "\n" + c.config.Region))
	return hex.EncodeToString(digest[:])
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Compute reboot request returned HTTP %d", e.StatusCode)
}

// New accepts a custom HTTP client for testing or a custom trust store. Redirects
// are always disabled, and the client is copied before setting its bounds.
func New(config Config, client *http.Client) (*Client, error) {
	base, err := secureURL(config.Endpoint)
	if err != nil {
		return nil, errors.New("Compute endpoint must be an HTTPS URL without credentials, query, or fragment")
	}
	if len(config.Project) < 2 || len(config.Project) > 19 || !projectPattern.MatchString(config.Project) {
		return nil, errors.New("Compute project must be a valid project slug")
	}
	if !namePattern.MatchString(config.Region) || strings.Contains(config.Region, ".") {
		return nil, errors.New("Compute region must be a nonempty region name")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("a positive Compute HTTP timeout is required")
	}
	if err := config.ValidateAuth(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{}
	}
	bounded := *client
	bounded.Timeout = config.Timeout
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	tokens, err := newCredentialSource(config, &bounded)
	if err != nil {
		return nil, err
	}
	return &Client{config: config, base: base, http: &bounded, tokens: tokens}, nil
}

// Prepare resolves credentials before the caller reserves a destructive action.
// An unavailable token exchange can then fail without consuming that reservation.
// This method never calls the reboot endpoint.
func (c *Client) Prepare(ctx context.Context) error {
	_, err := c.tokens.bearer(ctx)
	return err
}

// Reboot sends at most one request to the reboot endpoint. A transport error can
// mean the server already accepted it; the caller must journal the attempt first.
// Authentication failure never triggers an automatic retry of the reboot POST.
func (c *Client) Reboot(ctx context.Context, nodeName string) error {
	if len(nodeName) > 253 || !namePattern.MatchString(nodeName) {
		return errors.New("invalid Compute node name")
	}
	token, err := c.tokens.bearer(ctx)
	if err != nil {
		return err
	}
	target := *c.base
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/projects/" +
		c.config.Project + "/nodes/" + nodeName + "/reboot"
	target.RawPath = ""
	target.RawQuery = url.Values{"region": {c.config.Region}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		return errors.New("cannot construct Compute reboot request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nvsentinel-provider-mistral")
	response, err := c.http.Do(req)
	if err != nil {
		// Do not wrap url.Error: it can include a destination URL or transport detail.
		return errors.New("Compute reboot delivery was not confirmed; inspect the persisted attempt before retrying")
	}
	defer response.Body.Close()
	// Drain only a bounded amount; never log or return the body.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent {
		return &APIError{StatusCode: response.StatusCode}
	}
	return nil
}
