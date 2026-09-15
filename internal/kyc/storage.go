package kyc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// ErrObjectNotFound is returned by a DocumentStorageProvider's Retrieve
// when reference names no stored object.
var ErrObjectNotFound = errors.New("kyc: stored object not found")

// StoredObject is what Store returns - an opaque reference the SAME
// provider implementation later resolves via Retrieve. Never a public
// URL (directive §6/§8: "no public object URLs").
type StoredObject struct {
	Reference string
}

// DocumentStorageProvider is the storage boundary every implementation
// (mock or real) satisfies - directive §6's explicit requirement that
// the platform never hard-code a production cloud storage dependency.
// kyc_documents holds only the metadata this interface's own Store call
// returns (storage_provider = ID(), storage_reference =
// StoredObject.Reference) - never raw bytes.
//
// A real production implementation (documented, not built, this stage)
// would target private, encrypted object storage (e.g. S3 with SSE and a
// bucket policy denying public access) and have Retrieve mint a
// short-lived, signed access URL rather than returning bytes directly -
// see docs/decisions/0029 §3 for the full list of production
// requirements this mock does not implement (retention/legal-hold,
// malware-scanning integration at the storage layer itself, access
// logging at the object-storage layer).
type DocumentStorageProvider interface {
	ID() string
	Store(ctx context.Context, contentType string, content []byte) (StoredObject, error)
	Retrieve(ctx context.Context, reference string) (contentType string, content []byte, err error)
}

// MockDocumentStorageProvider is the only implementation this stage
// ships - an in-memory, process-local, non-persistent store. Deliberately
// NOT the local filesystem: writing uploaded content to disk anywhere
// under this repository's working tree would risk it landing in git or
// being picked up by an unrelated file-scanning tool, and an in-memory
// map is sufficient for "a mock/local implementation for development"
// (directive §6) while trivially guaranteeing neither problem can occur.
// Content is lost on process restart - fine for dev/test, explicitly not
// a production storage guarantee.
type MockDocumentStorageProvider struct {
	mu      sync.Mutex
	objects map[string]storedContent
}

type storedContent struct {
	contentType string
	content     []byte
}

func NewMockDocumentStorageProvider() *MockDocumentStorageProvider {
	return &MockDocumentStorageProvider{objects: make(map[string]storedContent)}
}

func (m *MockDocumentStorageProvider) ID() string { return "mock_memory" }

func (m *MockDocumentStorageProvider) Store(ctx context.Context, contentType string, content []byte) (StoredObject, error) {
	ref, err := randomReference()
	if err != nil {
		return StoredObject{}, err
	}
	stored := make([]byte, len(content))
	copy(stored, content)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[ref] = storedContent{contentType: contentType, content: stored}
	return StoredObject{Reference: ref}, nil
}

func (m *MockDocumentStorageProvider) Retrieve(ctx context.Context, reference string) (string, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[reference]
	if !ok {
		return "", nil, ErrObjectNotFound
	}
	out := make([]byte, len(obj.content))
	copy(out, obj.content)
	return obj.contentType, out, nil
}

func randomReference() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("kyc: generate storage reference: %w", err)
	}
	return hex.EncodeToString(b), nil
}
