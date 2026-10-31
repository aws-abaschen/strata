package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

type fakeArchive struct{ keys []string }

func (f *fakeArchive) Put(_ context.Context, r core.AuditRecord) error {
	f.keys = append(f.keys, Key(r))
	return nil
}

type fakePub struct {
	recs []core.AuditRecord
	err  error
}

func (f *fakePub) Publish(_ context.Context, recs []core.AuditRecord) error {
	f.recs = append(f.recs, recs...)
	return f.err
}

func streamRecord(t *testing.T, name, pk string, rec any) events.DynamoDBEventRecord {
	b, _ := json.Marshal(rec)
	return events.DynamoDBEventRecord{EventName: name, Change: events.DynamoDBStreamRecord{NewImage: map[string]events.DynamoDBAttributeValue{
		"PK":  events.NewStringAttribute(pk),
		"doc": events.NewStringAttribute(string(b)),
	}}}
}

func TestHandleArchivesThenPublishesAuditInserts(t *testing.T) {
	e, _ := core.NewAuditEntry("ChangeCreated", "ana", time.Now(), map[string]any{"repo": "payments", "subject": "PAY-1"})
	r1, _ := core.NextAuditRecord("payments", 0, "", e)
	r2, _ := core.NextAuditRecord("payments", r1.Seq, r1.Hash, e)
	ev := events.DynamoDBEvent{Records: []events.DynamoDBEventRecord{
		streamRecord(t, "INSERT", "AUDIT#payments", r1),
		streamRecord(t, "INSERT", "REPO#payments", map[string]string{"x": "not audit"}),
		streamRecord(t, "MODIFY", "AUDIT#payments", r1),
		streamRecord(t, "INSERT", "AUDIT#payments", r2),
	}}
	arch, pub := &fakeArchive{}, &fakePub{}
	if err := (&Ledger{Archive: arch, Publisher: pub}).Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(arch.keys) != 2 || arch.keys[1] != "audit/payments/000000000002.json" {
		t.Fatalf("archived %v", arch.keys)
	}
	if len(pub.recs) != 2 || pub.recs[0].Seq != 1 {
		t.Fatalf("published %v", pub.recs)
	}

	// A publish failure fails the batch so Lambda retries it.
	pub.err = errors.New("throttled")
	if err := (&Ledger{Archive: arch, Publisher: pub}).Handle(context.Background(), ev); err == nil {
		t.Fatal("want error")
	}
}

func TestEventDetailKeepsTheWritePayload(t *testing.T) {
	e, _ := core.NewAuditEntry("ChangeCreated", "ana", time.Now(), map[string]any{"repo": "payments", "metadata": map[string]any{"labels": []string{"x"}}})
	r, _ := core.NextAuditRecord("payments", 0, "", e)
	d, err := EventDetail(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal([]byte(d), &m)
	if m["repo"] != "payments" || m["metadata"] == nil || m["audit"].(map[string]any)["seq"].(float64) != 1 {
		t.Fatalf("detail %s", d)
	}
}
