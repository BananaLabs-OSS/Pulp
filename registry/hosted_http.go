package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type Authenticator interface {
	Authenticate(*http.Request) (Principal, error)
}

type AuthenticateFunc func(*http.Request) (Principal, error)

func (f AuthenticateFunc) Authenticate(r *http.Request) (Principal, error) { return f(r) }

type publishRequest struct {
	Manifest string            `json:"manifest"`
	Blobs    map[string][]byte `json:"blobs"`
}

// HostedHandler adds authenticated mutation and discovery endpoints to the
// read-only registry protocol. Request authentication is intentionally
// injectable (OIDC, mTLS, service tokens); Pulp does not store passwords.
func HostedHandler(service *HostedRegistry, auth Authenticator) http.Handler {
	read := Handler(service)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/", read)
	mux.HandleFunc("GET /v1/search", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		results, err := service.Search(r.Context(), r.URL.Query().Get("q"), limit)
		if registryHTTPError(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Results []SearchResult `json:"results"`
		}{results})
	})
	mux.HandleFunc("POST /v1/releases", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := authenticateHTTP(w, r, auth)
		if !ok {
			return
		}
		var request publishRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid publish request", http.StatusBadRequest)
			return
		}
		blobs := make(map[Digest][]byte, len(request.Blobs))
		for text, data := range request.Blobs {
			digest, err := ParseDigest(text)
			if err != nil || Sum(data) != digest {
				http.Error(w, "invalid blob digest", http.StatusBadRequest)
				return
			}
			blobs[digest] = data
		}
		if err := service.Publish(r.Context(), principal, []byte(request.Manifest), blobs); hostedHTTPError(w, err) {
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /v1/release-status", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := authenticateHTTP(w, r, auth)
		if !ok {
			return
		}
		var request struct {
			Module, Version, State, Reason string
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid status request", http.StatusBadRequest)
			return
		}
		id, err := ParseModuleID(request.Module)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		version, err := ParseVersion(request.Version)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		status := ReleaseStatus{State: ReleaseState(strings.ToLower(request.State)), Reason: strings.TrimSpace(request.Reason)}
		if err = service.SetReleaseStatus(r.Context(), principal, ReleaseID{ID: id, Version: version}, status); hostedHTTPError(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func authenticateHTTP(w http.ResponseWriter, r *http.Request, auth Authenticator) (Principal, bool) {
	if auth == nil {
		http.Error(w, "authentication unavailable", http.StatusUnauthorized)
		return Principal{}, false
	}
	principal, err := auth.Authenticate(r)
	if err != nil || strings.TrimSpace(principal.Subject) == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return Principal{}, false
	}
	return principal, true
}

func hostedHTTPError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrUnauthorized):
		http.Error(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrRevoked):
		http.Error(w, err.Error(), http.StatusGone)
	default:
		http.Error(w, fmt.Sprintf("registry: %v", err), http.StatusBadRequest)
	}
	return true
}
