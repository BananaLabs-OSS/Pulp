package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPStore is a read-only client for a hosted Pulp registry.
type HTTPStore struct {
	BaseURL string
	Client  *http.Client
}

func OpenHTTP(baseURL string, client *http.Client) (*HTTPStore, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid registry URL %q", baseURL)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("registry URL must use http or https")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPStore{BaseURL: parsed.String(), Client: client}, nil
}

func (s *HTTPStore) get(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		return nil, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("registry HTTP %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return response.Body, nil
}

func (s *HTTPStore) Versions(ctx context.Context, id ModuleID) ([]Version, error) {
	if _, err := ParseModuleID(string(id)); err != nil {
		return nil, err
	}
	body, err := s.get(ctx, "/v1/versions?module="+url.QueryEscape(string(id)))
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var wire struct {
		Versions []string `json:"versions"`
	}
	decoder := json.NewDecoder(io.LimitReader(body, 1<<20))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("decode registry versions: %w", err)
	}
	versions := make([]Version, len(wire.Versions))
	for i, value := range wire.Versions {
		if versions[i], err = ParseVersion(value); err != nil {
			return nil, fmt.Errorf("invalid upstream version: %w", err)
		}
	}
	return versions, nil
}

func (s *HTTPStore) Manifest(ctx context.Context, release ReleaseID) (Manifest, error) {
	body, err := s.get(ctx, "/v1/manifest?module="+url.QueryEscape(string(release.ID))+"&version="+url.QueryEscape(release.Version.String()))
	if err != nil {
		return Manifest{}, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := parseCanonical(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("decode registry manifest: %w", err)
	}
	if manifest.ID != release.ID || manifest.Version.Compare(release.Version) != 0 {
		return Manifest{}, errors.New("registry returned a different release")
	}
	return manifest, nil
}

func (s *HTTPStore) Blob(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	return s.get(ctx, "/v1/blobs/sha256/"+strings.TrimPrefix(digest.String(), "sha256:"))
}
