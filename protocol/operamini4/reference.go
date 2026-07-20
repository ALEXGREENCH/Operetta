package operamini4

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxReferenceBootstrapBytes = 4096

// ReferenceClient forwards decrypted OM4 application requests through a fresh
// transport session to a compatible reference endpoint. The opaque application
// header is kept intact, so server-side application sessions survive transport
// reconnects.
type ReferenceClient struct {
	endpoint  string
	client    *http.Client
	transport *http.Transport
}

// NewReferenceClient creates an isolated HTTP client for an explicitly enabled
// OM4 reference endpoint.
func NewReferenceClient(endpoint string) (*ReferenceClient, error) {
	endpoint = strings.TrimSpace(endpoint)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid OM4 reference URL %q", endpoint)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	return &ReferenceClient{
		endpoint:  parsed.String(),
		transport: transport,
		client:    &http.Client{Transport: transport},
	}, nil
}

// Exchange sends one decrypted client request to the reference endpoint and
// returns its authenticated, decrypted response frames.
func (client *ReferenceClient) Exchange(ctx context.Context, source *SessionRequest) ([]Frame, error) {
	if client == nil || client.client == nil {
		return nil, errors.New("nil OM4 reference client")
	}
	if source == nil {
		return nil, errors.New("nil OM4 source request")
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	hello := append([]byte{1, 1, 0}, nonce...)
	bootstrap, err := client.postBootstrap(ctx, hello)
	if err != nil {
		return nil, err
	}
	requestBody, session, err := BuildUpstreamRequest(bootstrap, nonce, source)
	if err != nil {
		return nil, fmt.Errorf("build reference request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := client.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reference request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("reference request status %s", resp.Status)
	}
	_, frames, err := session.ReadResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read reference response: %w", err)
	}
	return frames, nil
}

func (client *ReferenceClient) postBootstrap(ctx context.Context, hello []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(hello))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := client.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reference bootstrap: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reference bootstrap status %s", resp.Status)
	}
	bootstrap, err := io.ReadAll(io.LimitReader(resp.Body, maxReferenceBootstrapBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read reference bootstrap: %w", err)
	}
	if len(bootstrap) > maxReferenceBootstrapBytes {
		return nil, errors.New("reference bootstrap is too large")
	}
	return bootstrap, nil
}

// CloseIdleConnections releases pooled sockets owned by the reference client.
func (client *ReferenceClient) CloseIdleConnections() {
	if client != nil && client.transport != nil {
		client.transport.CloseIdleConnections()
	}
}
