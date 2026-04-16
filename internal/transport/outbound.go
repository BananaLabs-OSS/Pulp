package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/BananaLabs-OSS/Pulp/internal/abi"
)

// defaultFetchTimeout bounds a single outbound request. Plugins can still
// abort faster by the runtime canceling the parent context.
const defaultFetchTimeout = 30 * time.Second

// Fetcher performs outbound HTTP calls on behalf of plugins that declare
// transport.http.outbound. The underlying HTTP client is configured once
// at construction.
type Fetcher struct {
	client *http.Client
	logger *slog.Logger
}

// NewFetcher returns a Fetcher using an http.Client with a per-request
// timeout. Pass a nil logger to discard log output.
func NewFetcher(logger *slog.Logger) *Fetcher {
	return &Fetcher{
		client: &http.Client{Timeout: defaultFetchTimeout},
		logger: logger,
	}
}

// Do executes req and returns the response as an abi.HTTPResponse. Network
// errors, bad URLs, and timeouts are returned as errors — the caller is
// responsible for turning them into a non-zero status or a host-side code.
func (f *Fetcher) Do(ctx context.Context, req abi.HTTPFetchRequest) (abi.HTTPResponse, error) {
	if strings.TrimSpace(req.URL) == "" {
		return abi.HTTPResponse{}, errors.New("url is required")
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, body)
	if err != nil {
		return abi.HTTPResponse{}, fmt.Errorf("build request: %w", err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := f.client.Do(httpReq)
	if err != nil {
		return abi.HTTPResponse{}, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return abi.HTTPResponse{}, fmt.Errorf("read response body: %w", err)
	}

	headers := map[string]string{}
	for k, vs := range resp.Header {
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}

	return abi.HTTPResponse{
		Status:  uint32(resp.StatusCode),
		Headers: headers,
		Body:    respBody,
	}, nil
}
