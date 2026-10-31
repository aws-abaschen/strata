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
//
// With a standby region, the table is a global table: each region's stream
// carries every record, replicated ones included. Each region archives all
// of them to its own Object Lock bucket, but publishes only the records it
// wrote, so integrations get each event once.
type Ledger struct {
	Archive   Archive
	Publisher Publisher
	// Region is this region; records written elsewhere are archived but not
	// published. Empty: publish everything (one region).
	Region string
	// Primary is the region of records that carry no region (written before
	// the standby existed).
	Primary string
	// OnOverwrite sees an audit record that changed or disappeared. Records
	// are written once, so this is a fork (two regions wrote the same seq,
	// and replication kept one) or tampering. It must not fail the batch.
	OnOverwrite func(Overwrite)
}

// Overwrite is a changed or removed audit item.
type Overwrite struct {
	Key   string // PK/SK
	Event string // MODIFY or REMOVE
	Old   string // hash before, if the image has it
	New   string // hash after (MODIFY)
}

// Item is one inserted audit record and the region that wrote it.
type Item struct {
	Record core.AuditRecord
	Region string
}

// Records extracts audit records from a stream batch (inserts of AUDIT# items).
func Records(ev events.DynamoDBEvent) ([]core.AuditRecord, error) {
	items, _, err := parse(ev)
	out := make([]core.AuditRecord, len(items))
	for i, it := range items {
		out[i] = it.Record
	}
	return out, err
}

func auditKey(keys map[string]events.DynamoDBAttributeValue) (string, bool) {
	pk, ok := keys["PK"]
	if !ok || pk.DataType() != events.DataTypeString || !strings.HasPrefix(pk.String(), "AUDIT#") {
		return "", false
	}
	sk := keys["SK"]
	if sk.DataType() == events.DataTypeString {
		return pk.String() + "/" + sk.String(), true
	}
	return pk.String(), true
}

func docHash(img map[string]events.DynamoDBAttributeValue) string {
	doc, ok := img["doc"]
	if !ok || doc.DataType() != events.DataTypeString {
		return ""
	}
	var rec core.AuditRecord
	json.Unmarshal([]byte(doc.String()), &rec)
	return string(rec.Hash)
}

func parse(ev events.DynamoDBEvent) ([]Item, []Overwrite, error) {
	var items []Item
	var over []Overwrite
	for _, r := range ev.Records {
		keys := r.Change.Keys
		if len(keys) == 0 {
			keys = r.Change.NewImage
		}
		if len(keys) == 0 {
			keys = r.Change.OldImage
		}
		key, ok := auditKey(keys)
		if !ok {
			continue
		}
		switch r.EventName {
		case string(events.DynamoDBOperationTypeModify), string(events.DynamoDBOperationTypeRemove):
			over = append(over, Overwrite{Key: key, Event: r.EventName, Old: docHash(r.Change.OldImage), New: docHash(r.Change.NewImage)})
			continue
		case string(events.DynamoDBOperationTypeInsert):
		default:
			continue
		}
		img := r.Change.NewImage
		doc, ok := img["doc"]
		if !ok || doc.DataType() != events.DataTypeString {
			return nil, nil, fmt.Errorf("audit item %s without doc", key)
		}
		var rec core.AuditRecord
		if err := json.Unmarshal([]byte(doc.String()), &rec); err != nil {
			return nil, nil, fmt.Errorf("audit item %s: %w", key, err)
		}
		region := ""
		if rg, ok := img["region"]; ok && rg.DataType() == events.DataTypeString {
			region = rg.String()
		}
		items = append(items, Item{Record: rec, Region: region})
	}
	return items, over, nil
}

// Handle archives then publishes. Any error fails the batch, and Lambda
// retries it; both steps are idempotent.
func (l *Ledger) Handle(ctx context.Context, ev events.DynamoDBEvent) error {
	items, over, err := parse(ev)
	if err != nil {
		return err
	}
	for _, o := range over {
		if l.OnOverwrite != nil {
			l.OnOverwrite(o)
		}
	}
	var own []core.AuditRecord
	for _, it := range items {
		r := it.Record
		if err := l.Archive.Put(ctx, r); err != nil {
			return fmt.Errorf("archive %s#%d: %w", r.Repo, r.Seq, err)
		}
		region := it.Region
		if region == "" {
			region = l.Primary
		}
		if l.Region == "" || region == "" || region == l.Region {
			own = append(own, r)
		}
	}
	if len(own) == 0 {
		return nil
	}
	return l.Publisher.Publish(ctx, own)
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
