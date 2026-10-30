// Package store defines Strata's storage interfaces and their implementations:
// S3 for content (objects and sealed changes) and DynamoDB for the index
// (repos, refs, change pointers, work-item and label lookups).
package store

import (
	"context"
	"errors"
	"net/http"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

var (
	// ErrNotFound means the item does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists means a create found the item already there.
	ErrExists = errors.New("already exists")
	// ErrConflict means a compare-and-swap lost a race (a ref or change moved).
	ErrConflict = errors.New("conflict")
)

// Upload is a presigned request the client sends to store one object.
type Upload struct {
	Hash    core.Hash         `json:"hash"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
}

// Objects stores immutable, content-addressed bytes.
type Objects interface {
	Exists(ctx context.Context, repo string, h core.Hash) (bool, error)
	// Get reads a small object (trees, sealed changes). max caps the size read.
	Get(ctx context.Context, repo string, h core.Hash, max int64) ([]byte, error)
	// PresignPut lets a client upload an object directly. The store must
	// reject bytes that do not hash to h.
	PresignPut(ctx context.Context, repo string, h core.Hash, size int64) (Upload, error)
	PresignGet(ctx context.Context, repo string, h core.Hash) (string, error)
	// PutChange writes a sealed change document, once.
	PutChangeDoc(ctx context.Context, c core.Change, doc []byte) error
}

// RefUpdate moves a ref as part of writing a change. Expected is the target
// the caller saw; empty means the ref must not exist yet.
type RefUpdate struct {
	Name     string
	Expected core.Hash
}

// Meta is the index of repositories, refs and changes.
type Meta interface {
	CreateRepo(ctx context.Context, r core.Repo) error
	GetRepo(ctx context.Context, name string) (core.Repo, error)
	ListRepos(ctx context.Context) ([]core.Repo, error)
	// UpdateRepo saves settings with compare-and-swap on the Layer the repo
	// extended before (nil = none), and keeps the subscriber index in step.
	UpdateRepo(ctx context.Context, r core.Repo, previous *core.LayerRef) error
	// ListSubscribers returns the repositories that directly extend a Layer.
	ListSubscribers(ctx context.Context, layer string) ([]core.Repo, error)

	ListRefs(ctx context.Context, repo string) ([]core.Ref, error)
	GetRef(ctx context.Context, repo, name string) (core.Ref, error)
	// UpdateRef moves a ref with compare-and-swap. An empty target deletes it.
	UpdateRef(ctx context.Context, repo string, ref core.Ref, expected core.Hash) error

	// PutChange records a sealed change atomically: the revision, the change
	// id pointer (which must still point at c.Predecessor), the work-item and
	// label index entries, and the optional ref move.
	PutChange(ctx context.Context, c core.Change, ref *RefUpdate) error
	GetChange(ctx context.Context, repo string, rev core.Hash) (core.Change, error)
	// LatestRevision returns the newest revision of a change id.
	LatestRevision(ctx context.Context, repo, changeID string) (core.Hash, error)

	PutAnnotation(ctx context.Context, a core.Annotation) error
	ListAnnotations(ctx context.Context, repo string, rev core.Hash) ([]core.Annotation, error)

	// FindByWorkItem and FindByLabel search across all repositories, newest first.
	FindByWorkItem(ctx context.Context, system, key string, limit int) ([]core.ChangeSummary, error)
	FindByLabel(ctx context.Context, label string, limit int) ([]core.ChangeSummary, error)
}

// Events publishes domain events (to EventBridge in AWS).
type Events interface {
	Publish(ctx context.Context, detailType string, detail any) error
}

// NoEvents drops every event.
type NoEvents struct{}

// Publish does nothing.
func (NoEvents) Publish(context.Context, string, any) error { return nil }

// headerMap flattens signed headers for the client, minus Host.
func headerMap(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if http.CanonicalHeaderKey(k) == "Host" || len(v) == 0 {
			continue
		}
		out[k] = v[0]
	}
	return out
}
