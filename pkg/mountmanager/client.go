package mountmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to the mount service over a Unix domain socket.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient builds a new Client for the given endpoint.
func NewClient(endpoint string) (*Client, error) {
	scheme, address, err := ParseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if scheme != "unix" {
		return nil, fmt.Errorf("unsupported endpoint scheme: %s", scheme)
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", address)
		},
	}

	return &Client{
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
		baseURL: "http://unix",
	}, nil
}

// Mount mounts a volume using the mount service.
func (c *Client) Mount(req *MountRequest) (*MountResponse, error) {
	var resp MountResponse
	if err := c.doPost("/mount", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Unmount unmounts a volume using the mount service.
func (c *Client) Unmount(req *UnmountRequest) (*UnmountResponse, error) {
	var resp UnmountResponse
	if err := c.doPost("/unmount", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ErrListUnsupported is returned when the mount service predates /list. The
// two images roll independently, so a new node plugin routinely meets an old
// mount service; callers degrade to an empty volume map rather than failing.
var ErrListUnsupported = errors.New("mount service does not support /list")

// List returns the volumes the mount service currently owns.
func (c *Client) List() (*ListResponse, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/list", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call mount service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, ErrListUnsupported
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mount service error: %s (%s)", resp.Status, string(data))
	}

	var out ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

func (c *Client) doPost(path string, payload any, out any) error {
	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call mount service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err == nil && errResp.Error != "" {
			return errors.New(errResp.Error)
		}
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("mount service error: %s (failed to read body: %v)", resp.Status, readErr)
		}
		return fmt.Errorf("mount service error: %s (%s)", resp.Status, string(data))
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
