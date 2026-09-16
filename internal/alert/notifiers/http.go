// Package notifiers contains the alert destination implementations. Each
// notifier is small: a config type (from internal/config), a Send method, and a
// Test method for `cometduty test-alert`.
package notifiers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var httpClient = &http.Client{Timeout: 20 * time.Second}

// postJSON marshals body and POSTs it, requiring a 2xx response.
func postJSON(ctx context.Context, url string, headers map[string]string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return post(ctx, url, "application/json", headers, data)
}

// post sends a raw body with headers, requiring a 2xx response.
func post(ctx context.Context, url, contentType string, headers map[string]string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: http %d: %s", url, resp.StatusCode, string(b))
	}
	return nil
}
