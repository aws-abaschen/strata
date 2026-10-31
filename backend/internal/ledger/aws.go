package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// S3Archive writes audit/<repo>/<seq>.json once. The bucket's default
// Object Lock retention protects it.
type S3Archive struct {
	Client *s3.Client
	Bucket string
}

// Key is the archive key of a record.
func Key(r core.AuditRecord) string { return fmt.Sprintf("audit/%s/%012d.json", r.Repo, r.Seq) }

// Put implements Archive.
func (a *S3Archive) Put(ctx context.Context, r core.AuditRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = a.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &a.Bucket, Key: aws.String(Key(r)), Body: bytes.NewReader(b),
		ContentType: aws.String("application/json"), IfNoneMatch: aws.String("*"),
	})
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusPreconditionFailed {
		return nil // archived by an earlier attempt
	}
	return err
}

// EventBridgePublisher sends records to a bus, 10 per call.
type EventBridgePublisher struct {
	Client *eventbridge.Client
	Bus    string
}

// Publish implements Publisher.
func (p *EventBridgePublisher) Publish(ctx context.Context, recs []core.AuditRecord) error {
	for i := 0; i < len(recs); i += 10 {
		var entries []ebtypes.PutEventsRequestEntry
		for _, r := range recs[i:min(i+10, len(recs))] {
			d, err := EventDetail(r)
			if err != nil {
				return err
			}
			entries = append(entries, ebtypes.PutEventsRequestEntry{
				EventBusName: aws.String(p.Bus), Source: aws.String("strata"), DetailType: aws.String(r.Action),
				Detail: aws.String(d), Time: aws.Time(r.At),
			})
		}
		out, err := p.Client.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: entries})
		if err != nil {
			return err
		}
		if out.FailedEntryCount > 0 {
			for _, e := range out.Entries {
				if e.ErrorCode != nil {
					return fmt.Errorf("eventbridge: %s %s", aws.ToString(e.ErrorCode), aws.ToString(e.ErrorMessage))
				}
			}
		}
	}
	return nil
}

// Get reads an archived record (store.ErrNotFound if missing).
func (a *S3Archive) Get(ctx context.Context, repo string, seq int64) (core.AuditRecord, error) {
	var r core.AuditRecord
	out, err := a.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: &a.Bucket, Key: aws.String(Key(core.AuditRecord{Repo: repo, Seq: seq}))})
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return r, store.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	defer out.Body.Close()
	return r, json.NewDecoder(io.LimitReader(out.Body, 1<<20)).Decode(&r)
}

// PutAnchor writes anchors/<seq>.json once, under the bucket's Object Lock.
func (a *S3Archive) PutAnchor(ctx context.Context, x core.Anchor) error {
	b, err := json.Marshal(x)
	if err != nil {
		return err
	}
	_, err = a.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &a.Bucket, Key: aws.String(fmt.Sprintf("anchors/%012d.json", x.Statement.Seq)), Body: bytes.NewReader(b),
		ContentType: aws.String("application/json"), IfNoneMatch: aws.String("*"),
	})
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusPreconditionFailed {
		return nil
	}
	return err
}
