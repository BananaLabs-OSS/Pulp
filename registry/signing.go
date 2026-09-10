package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// SignedPublication proves possession of an accountless Ed25519 publisher
// key. The signature binds the canonical manifest and every declared artifact.
type SignedPublication struct {
	Manifest  []byte
	Blobs     map[Digest][]byte
	PublicKey ed25519.PublicKey
	Signature []byte
}

func PublisherID(public ed25519.PublicKey) (string, error) {
	if len(public) != ed25519.PublicKeySize {
		return "", errors.New("publisher key must be Ed25519")
	}
	digest := sha256.Sum256(public)
	return "ed25519:" + hex.EncodeToString(digest[:]), nil
}

func PublicationSigningBytes(manifestBytes []byte) ([]byte, error) {
	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return nil, err
	}
	canonical, err := CanonicalManifest(manifest)
	if err != nil {
		return nil, err
	}
	type artifact struct {
		Target, Digest string
		Size           int64
	}
	artifacts := make([]artifact, len(manifest.Artifacts))
	for i, item := range manifest.Artifacts {
		artifacts[i] = artifact{item.Target, item.Digest.String(), item.Size}
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Target < artifacts[j].Target })
	return json.Marshal(struct {
		Schema    int        `json:"schema"`
		Manifest  Digest     `json:"manifest"`
		Artifacts []artifact `json:"artifacts"`
	}{1, Sum(canonical), artifacts})
}

func SignPublication(private ed25519.PrivateKey, manifest []byte, blobs map[Digest][]byte) (SignedPublication, error) {
	if len(private) != ed25519.PrivateKeySize {
		return SignedPublication{}, errors.New("publisher private key must be Ed25519")
	}
	message, err := PublicationSigningBytes(manifest)
	if err != nil {
		return SignedPublication{}, err
	}
	copyBlobs := make(map[Digest][]byte, len(blobs))
	for digest, data := range blobs {
		if Sum(data) != digest {
			return SignedPublication{}, fmt.Errorf("blob %s digest mismatch", digest)
		}
		copyBlobs[digest] = append([]byte(nil), data...)
	}
	return SignedPublication{Manifest: append([]byte(nil), manifest...), Blobs: copyBlobs, PublicKey: append(ed25519.PublicKey(nil), private.Public().(ed25519.PublicKey)...), Signature: ed25519.Sign(private, message)}, nil
}

func (s *HostedRegistry) PublishSigned(ctx context.Context, publication SignedPublication) error {
	publisher, err := PublisherID(publication.PublicKey)
	if err != nil {
		return err
	}
	message, err := PublicationSigningBytes(publication.Manifest)
	if err != nil {
		return err
	}
	if len(publication.Signature) != ed25519.SignatureSize || !ed25519.Verify(publication.PublicKey, message, publication.Signature) {
		return errors.New("invalid publication signature")
	}
	return s.Publish(ctx, Principal{Subject: publisher}, publication.Manifest, publication.Blobs)
}
