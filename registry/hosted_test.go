package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func hostedFixture(t *testing.T) (*HostedRegistry, Manifest, []byte, []byte) {
	t.Helper()
	payload := []byte("wasm package")
	digest := Sum(payload)
	id, _ := ParseModuleID("acme/render")
	version, _ := ParseVersion("1.2.3")
	manifest := Manifest{SchemaVersion: 1, ID: id, Version: version, Artifacts: []Artifact{{Target: "wasm", Digest: digest, DigestText: digest.String(), Size: int64(len(payload))}}}
	wire, err := ManifestTOML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	local, err := OpenLocal(filepath.Join(t.TempDir(), "registry"))
	if err != nil {
		t.Fatal(err)
	}
	service := &HostedRegistry{
		Storage: local, Publisher: local, Metadata: NewMemoryMetadata(),
		Authorize: AuthorizeFunc(func(_ context.Context, principal Principal, _ Operation, id ModuleID) error {
			if principal.Subject != "alice" || id != "acme/render" {
				return errors.New("namespace is owned by alice")
			}
			return nil
		}),
	}
	return service, manifest, wire, payload
}

func TestHostedRegistryPublishYankRevokeAndSearch(t *testing.T) {
	ctx := context.Background()
	service, manifest, wire, payload := hostedFixture(t)
	digest := manifest.Artifacts[0].Digest
	if err := service.Publish(ctx, Principal{Subject: "mallory"}, wire, map[Digest][]byte{digest: payload}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unauthorized publish = %v", err)
	}
	if err := service.Publish(ctx, Principal{Subject: "alice"}, wire, map[Digest][]byte{digest: payload}); err != nil {
		t.Fatal(err)
	}
	results, err := service.Search(ctx, "render", 10)
	if err != nil || len(results) != 1 || results[0].LatestVersion != "1.2.3" {
		t.Fatalf("search = %#v, %v", results, err)
	}
	release := ReleaseID{ID: manifest.ID, Version: manifest.Version}
	if err = service.SetReleaseStatus(ctx, Principal{Subject: "alice"}, release, ReleaseStatus{State: ReleaseYanked, Reason: "broken"}); err != nil {
		t.Fatal(err)
	}
	versions, err := service.Versions(ctx, manifest.ID)
	if err != nil || len(versions) != 0 {
		t.Fatalf("yanked versions = %#v, %v", versions, err)
	}
	if _, err = service.Manifest(ctx, release); err != nil {
		t.Fatalf("locked yanked release must remain readable: %v", err)
	}
	if err = service.SetReleaseStatus(ctx, Principal{Subject: "alice"}, release, ReleaseStatus{State: ReleaseRevoked, Reason: "malware"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Manifest(ctx, release); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked manifest = %v", err)
	}
	if _, err = service.Blob(ctx, digest); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked blob = %v", err)
	}
}

func TestHostedHandlerAuthenticationPublishingAndRevocation(t *testing.T) {
	service, manifest, wire, payload := hostedFixture(t)
	auth := AuthenticateFunc(func(request *http.Request) (Principal, error) {
		if request.Header.Get("Authorization") != "Bearer good" {
			return Principal{}, ErrUnauthorized
		}
		return Principal{Subject: "alice"}, nil
	})
	handler := HostedHandler(service, auth)
	digest := manifest.Artifacts[0].Digest
	body, _ := json.Marshal(publishRequest{Manifest: string(wire), Blobs: map[string][]byte{digest.String(): payload}})

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/releases", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, unauthorized)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.Code)
	}

	publish := httptest.NewRequest(http.MethodPost, "/v1/releases", bytes.NewReader(body))
	publish.Header.Set("Authorization", "Bearer good")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, publish)
	if response.Code != http.StatusCreated {
		t.Fatalf("publish status = %d: %s", response.Code, response.Body.String())
	}

	search := httptest.NewRequest(http.MethodGet, "/v1/search?q=acme", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, search)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"id":"acme/render"`)) {
		t.Fatalf("search = %d: %s", response.Code, response.Body.String())
	}

	statusBody := bytes.NewBufferString(`{"Module":"acme/render","Version":"1.2.3","State":"revoked","Reason":"compromised"}`)
	status := httptest.NewRequest(http.MethodPost, "/v1/release-status", statusBody)
	status.Header.Set("Authorization", "Bearer good")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, status)
	if response.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d: %s", response.Code, response.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/v1/manifest?module=acme%2Frender&version=1.2.3", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, get)
	if response.Code != http.StatusGone {
		t.Fatalf("revoked GET = %d: %s", response.Code, response.Body.String())
	}
	reader, err := service.Storage.Blob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got, _ := io.ReadAll(reader); !bytes.Equal(got, payload) {
		t.Fatalf("immutable blob changed")
	}
}
