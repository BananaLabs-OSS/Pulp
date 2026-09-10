package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPPublisher is the credentialed write half of a hosted registry. Keep it
// separate from HTTPStore so read-only runtimes cannot publish merely because
// they can resolve modules.
type HTTPPublisher struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func OpenHTTPPublisher(baseURL, token string, client *http.Client) (*HTTPPublisher, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid registry URL %q", baseURL)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("registry URL must use http or https")
	}
	if len(strings.TrimSpace(token)) < 32 {
		return nil, fmt.Errorf("registry publishing token must contain at least 32 bytes")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPPublisher{BaseURL: parsed.String(), Token: strings.TrimSpace(token), Client: client}, nil
}

func (p *HTTPPublisher) Publish(ctx context.Context, manifest []byte, blobs map[Digest][]byte) error {
	if p == nil || p.Client == nil || p.BaseURL == "" || p.Token == "" {
		return fmt.Errorf("HTTP publisher is not configured")
	}
	wireBlobs := make(map[string][]byte, len(blobs))
	for digest, data := range blobs {
		if Sum(data) != digest {
			return fmt.Errorf("blob %s does not match content digest", digest)
		}
		wireBlobs[digest.String()] = data
	}
	body, err := json.Marshal(publishRequest{Manifest: string(manifest), Blobs: wireBlobs})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/releases", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+p.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("registry publish HTTP %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return nil
}
