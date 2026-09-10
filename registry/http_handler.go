package registry

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Handler exposes the immutable, read-only hosted-registry protocol for any
// Store implementation. Publishing/authentication can be layered separately.
func Handler(store Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/versions", func(w http.ResponseWriter, r *http.Request) {
		id, err := ParseModuleID(r.URL.Query().Get("module"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		versions, err := store.Versions(r.Context(), id)
		if registryHTTPError(w, err) {
			return
		}
		values := make([]string, len(versions))
		for i, version := range versions {
			values[i] = version.String()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Versions []string `json:"versions"`
		}{values})
	})
	mux.HandleFunc("GET /v1/manifest", func(w http.ResponseWriter, r *http.Request) {
		id, err := ParseModuleID(r.URL.Query().Get("module"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		version, err := ParseVersion(r.URL.Query().Get("version"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		manifest, err := store.Manifest(r.Context(), ReleaseID{ID: id, Version: version})
		if registryHTTPError(w, err) {
			return
		}
		data, err := CanonicalManifest(manifest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})
	mux.HandleFunc("GET /v1/blobs/sha256/{digest}", func(w http.ResponseWriter, r *http.Request) {
		digest, err := ParseDigest("sha256:" + strings.TrimSpace(r.PathValue("digest")))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reader, err := store.Blob(r.Context(), digest)
		if registryHTTPError(w, err) {
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "application/wasm")
		w.Header().Set("ETag", `"`+digest.String()+`"`)
		io.Copy(w, reader)
	})
	return mux
}

func registryHTTPError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
	} else if errors.Is(err, ErrRevoked) {
		http.Error(w, "release revoked", http.StatusGone)
	} else {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	return true
}
