// Package ledger consumes the audit records DynamoDB streams out of the
// index table. For each new record it (1) archives it to an S3 bucket with
// Object Lock, so even an administrator cannot alter or delete it during
// retention, and (2) publishes it to EventBridge, so integrations receive
// every committed write: the ledger is the event outbox.
package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Archive stores a record immutably. Storing the same record twice must succeed.
type Archive interface {
	Put(ctx context.Context, r core.AuditRecord) error
}

// Publisher sends records as events, in order. At-least-once is fine.
type Publisher interface {
	Publish(ctx context.Context, recs []core.AuditRecord) error
}

// Ledger is the stream handler.
type Ledger struct {
	Archive   Archive
	Publisher Publisher
}

// Records extracts audit records from a stream batch (inserts of AUDIT# items).
func Records(ev events.DynamoDBEvent) ([]core.AuditRecord, error) {
	var out []core.AuditRecord
	for _, r := range ev.Records {
		if r.EventName != string(events.DynamoDBOperationTypeInsert) {
			continue
		}
		img := r.Change.NewImage
		pk, ok := img["PK"]
		if !ok || pk.DataType() != events.DataTypeString || !strings.HasPrefix(pk.String(), "AUDIT#") {
			continue
		}
		doc, ok := img["doc"]
		if !ok || doc.DataType() != events.DataTypeString {
			return nil, fmt.Errorf("audit item %s without doc", pk.String())
		}
		var rec core.AuditRecord
		if err := json.Unmarshal([]byte(doc.String()), &rec); err != nil {
			return nil, fmt.Errorf("audit item %s: %w", pk.String(), err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// Handle archives then publishes. Any error fails the batch, and Lambda
// retries it; both steps are idempotent.
func (l *Ledger) Handle(ctx context.Context, ev events.DynamoDBEvent) error {
	recs, err := Records(ev)
	if err != nil || len(recs) == 0 {
		return err
	}
	for _, r := range recs {
		if err := l.Archive.Put(ctx, r); err != nil {
			return fmt.Errorf("archive %s#%d: %w", r.Repo, r.Seq, err)
		}
	}
	return l.Publisher.Publish(ctx, recs)
}

// EventDetail is what integrations receive: the write's own detail plus a
// pointer to its ledger record.
func EventDetail(r core.AuditRecord) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(r.Detail, &m); err != nil || m == nil {
		m = map[string]any{"detail": json.RawMessage(r.Detail)}
	}
	m["audit"] = map[string]any{"repo": r.Repo, "seq": r.Seq, "hash": r.Hash, "actor": r.Actor, "at": r.At}
	b, err := json.Marshal(m)
	return string(b), err
}
