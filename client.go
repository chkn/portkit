package portkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// adminClient talks to Caddy's admin API, either over the admin socket (the
// CLI) or in-process over dialSelf (the plugin changing its own config).
type adminClient struct {
	hc *http.Client
}

func newSocketClient(path string) *adminClient {
	return &adminClient{hc: &http.Client{
		Timeout: 90 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}}
}

func newSelfClient() *adminClient {
	return &adminClient{hc: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dialSelf(ctx) },
			DisableKeepAlives: true,
		},
	}}
}

// adminError is an error response from the admin API.
type adminError struct {
	Status  int
	Message string
}

func (e *adminError) Error() string { return e.Message }

// do sends a request with an optional JSON body and decodes a JSON response
// into out, if out is non-nil.
func (c *adminClient) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	// The admin API checks the Host header; bootstrap.json allows 127.0.0.1.
	req, err := http.NewRequest(method, "http://127.0.0.1"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		// A rejected config change reports the whole provisioning chain;
		// keep only portkit's own message.
		if _, rest, ok := strings.Cut(msg, "provision portkit: "); ok {
			msg = rest
		}
		return &adminError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding %s %s: %w", method, path, err)
		}
	}
	return nil
}

// servicePath is the config path of one service record.
func servicePath(name string) string { return "/config/apps/portkit/services/" + name }
