package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client is a minimal AT Protocol client: app-password login, token refresh,
// and record writes against the user's own PDS.
type Client struct {
	identifier string // handle or DID
	password   string
	pds        string // https://host, resolved from the DID document if empty

	http *http.Client

	mu      sync.Mutex
	did     string
	access  string
	refresh string
}

type xrpcError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *xrpcError) Error() string {
	return fmt.Sprintf("xrpc %d %s: %s", e.Status, e.Code, e.Message)
}

func NewClient(identifier, password, pds string) *Client {
	return &Client{
		identifier: strings.TrimPrefix(identifier, "@"),
		password:   password,
		pds:        strings.TrimRight(pds, "/"),
		http:       &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) DID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.did
}

// Login resolves the PDS (if needed) and creates a session.
func (c *Client) Login(ctx context.Context) error {
	if c.pds == "" {
		did, err := resolveHandle(ctx, c.http, c.identifier)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", c.identifier, err)
		}
		pds, err := resolvePDS(ctx, c.http, did)
		if err != nil {
			return fmt.Errorf("resolving PDS for %s: %w", did, err)
		}
		c.pds = pds
	}
	var out struct {
		DID        string `json:"did"`
		AccessJwt  string `json:"accessJwt"`
		RefreshJwt string `json:"refreshJwt"`
	}
	err := c.do(ctx, "com.atproto.server.createSession", "", map[string]string{
		"identifier": c.identifier,
		"password":   c.password,
	}, &out)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.did, c.access, c.refresh = out.DID, out.AccessJwt, out.RefreshJwt
	c.mu.Unlock()
	return nil
}

func (c *Client) refreshSession(ctx context.Context) error {
	c.mu.Lock()
	tok := c.refresh
	c.mu.Unlock()
	var out struct {
		AccessJwt  string `json:"accessJwt"`
		RefreshJwt string `json:"refreshJwt"`
	}
	if err := c.do(ctx, "com.atproto.server.refreshSession", tok, nil, &out); err != nil {
		return c.Login(ctx) // refresh token expired too; start over
	}
	c.mu.Lock()
	c.access, c.refresh = out.AccessJwt, out.RefreshJwt
	c.mu.Unlock()
	return nil
}

// authed calls a procedure with the access token, refreshing once on expiry.
func (c *Client) authed(ctx context.Context, nsid string, body, out any) error {
	c.mu.Lock()
	tok := c.access
	c.mu.Unlock()
	err := c.do(ctx, nsid, tok, body, out)
	var xe *xrpcError
	if errors.As(err, &xe) && (xe.Status == 401 || xe.Code == "ExpiredToken" || xe.Code == "InvalidToken") {
		if err := c.refreshSession(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		tok = c.access
		c.mu.Unlock()
		return c.do(ctx, nsid, tok, body, out)
	}
	return err
}

// PutRecord creates or replaces a record. Using a caller-chosen rkey makes
// retries idempotent.
func (c *Client) PutRecord(ctx context.Context, collection, rkey string, record any) (string, error) {
	var out struct {
		URI string `json:"uri"`
	}
	err := c.authed(ctx, "com.atproto.repo.putRecord", map[string]any{
		"repo":       c.DID(),
		"collection": collection,
		"rkey":       rkey,
		"record":     record,
	}, &out)
	return out.URI, err
}

func (c *Client) do(ctx context.Context, nsid, token string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.pds+"/xrpc/"+nsid, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		xe := &xrpcError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, xe)
		return xe
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// isPermanent reports whether retrying a write can never succeed (as opposed to
// network trouble or a server hiccup).
func isPermanent(err error) bool {
	var xe *xrpcError
	if !errors.As(err, &xe) {
		return false
	}
	return xe.Status == 400 && xe.Code == "InvalidRequest"
}

func resolveHandle(ctx context.Context, hc *http.Client, handle string) (string, error) {
	if strings.HasPrefix(handle, "did:") {
		return handle, nil
	}
	var r net.Resolver
	if txts, err := r.LookupTXT(ctx, "_atproto."+handle); err == nil {
		for _, t := range txts {
			if did, ok := strings.CutPrefix(t, "did="); ok {
				return did, nil
			}
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+handle+"/.well-known/atproto-did", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	did := strings.TrimSpace(string(b))
	if resp.StatusCode != 200 || !strings.HasPrefix(did, "did:") {
		return "", fmt.Errorf("handle did not resolve (DNS TXT and /.well-known both failed)")
	}
	return did, nil
}

func resolvePDS(ctx context.Context, hc *http.Client, did string) (string, error) {
	var url string
	switch {
	case strings.HasPrefix(did, "did:plc:"):
		url = "https://plc.directory/" + did
	case strings.HasPrefix(did, "did:web:"):
		url = "https://" + strings.TrimPrefix(did, "did:web:") + "/.well-known/did.json"
	default:
		return "", fmt.Errorf("unsupported DID method: %s", did)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var doc struct {
		Service []struct {
			ID              string `json:"id"`
			Type            string `json:"type"`
			ServiceEndpoint string `json:"serviceEndpoint"`
		} `json:"service"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	for _, s := range doc.Service {
		if strings.HasSuffix(s.ID, "#atproto_pds") || s.Type == "AtprotoPersonalDataServer" {
			return strings.TrimRight(s.ServiceEndpoint, "/"), nil
		}
	}
	return "", fmt.Errorf("no PDS in DID document")
}

// newTID returns an AT Protocol timestamp identifier for t: 53 bits of
// microseconds since the epoch plus a 10-bit clock id, base32-sortable.
func newTID(t time.Time, clockID uint64) string {
	const alphabet = "234567abcdefghijklmnopqrstuvwxyz"
	v := (uint64(t.UnixMicro())&((1<<53)-1))<<10 | (clockID & 0x3ff)
	var b [13]byte
	for i := 12; i >= 0; i-- {
		b[i] = alphabet[v&31]
		v >>= 5
	}
	return string(b[:])
}
