package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestSignedPublicationProvesAccountlessPublisher(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	publisher, _ := PublisherID(public)
	blob := []byte("module")
	manifest := []byte("schema_version=1\nprovides=[]\nconsumes=[]\ncapabilities=[]\n[module]\nid=\"demo/signed\"\nversion=\"1.0.0\"\n[[artifacts]]\ntarget=\"wasm\"\ndigest=\"" + Sum(blob).String() + "\"\nsize=6\n")
	publication, err := SignPublication(private, manifest, map[Digest][]byte{Sum(blob): blob})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := OpenLocal(t.TempDir())
	hosted := &HostedRegistry{Storage: local, Publisher: local, Metadata: NewMemoryMetadata(), Authorize: AuthorizeFunc(func(_ context.Context, principal Principal, _ Operation, _ ModuleID) error {
		if principal.Subject != publisher {
			return ErrForbidden
		}
		return nil
	})}
	if err = hosted.PublishSigned(context.Background(), publication); err != nil {
		t.Fatal(err)
	}
	publication.Signature[0] ^= 0xff
	if err = hosted.PublishSigned(context.Background(), publication); err == nil {
		t.Fatal("tampered signed publication accepted")
	}
}
