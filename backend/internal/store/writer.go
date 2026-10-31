package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Writer names the one region allowed to write. The table is replicated to a
// standby region (DynamoDB global table, asynchronous, last writer wins), and
// two regions writing at once would fork every audit chain. So every audited
// write checks, in its own transaction, that this region is the writer.
//
//	PK=SYSTEM SK=WRITER   region, epoch (CAS), doc
//
// Before the item exists, only the primary region may write.
type Writer struct {
	Region string    `json:"region"`
	Epoch  int64     `json:"epoch"`
	API    string    `json:"api,omitempty"` // the writer region's API URL, for clients
	Since  time.Time `json:"since"`
	By     string    `json:"by,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// ErrNotWriter means this region is not the writer.
var ErrNotWriter = errors.New("not the writer region")

// NotWriterError says where writes go instead.
type NotWriterError struct {
	Here   string
	Writer Writer
}

func (e *NotWriterError) Error() string {
	return fmt.Sprintf("%s is not the writer region: writes go to %s (epoch %d)", e.Here, e.Writer.Region, e.Writer.Epoch)
}

func (e *NotWriterError) Unwrap() error { return ErrNotWriter }

// Fence is the writer record and what promotion needs.
type Fence interface {
	// Writer returns the current writer as this region sees it. Without a
	// writer item it returns the primary region at epoch 0.
	Writer(ctx context.Context) (Writer, error)
	// SetWriter replaces the writer if its epoch is still expectEpoch
	// (0: no writer item yet). ErrConflict otherwise.
	SetWriter(ctx context.Context, w Writer, expectEpoch int64) error
	// Writable returns a NotWriterError unless this region is the writer.
	// For writes that are not audited (upload URLs), so clients are sent to
	// the writer before they upload anything.
	Writable(ctx context.Context) error
	// AppendAudit writes an audit record on its own (promotion records,
	// the DR chain).
	AppendAudit(ctx context.Context, repo string, audit core.AuditEntry) error
	// InspectChain reports a chain's head and records around it, so
	// promotion can repair a partly replicated chain.
	InspectChain(ctx context.Context, repo string) (ChainState, error)
	// SetAuditHead moves a chain's head to seq/hash if it is still at
	// expectSeq. Promotion only.
	SetAuditHead(ctx context.Context, repo string, seq int64, hash core.Hash, expectSeq int64) error
	// QuarantineAudit moves records off a chain (to ORPHAN#<repo>, kept as
	// evidence), so the chain can continue past them. Promotion only.
	QuarantineAudit(ctx context.Context, repo string, seqs []int64, epoch int64) error
}

// ChainState is a chain as one region's table holds it. After a forced
// promotion the standby may hold part of a transaction: a head without its
// record (records lost) or records without their head update (head behind).
type ChainState struct {
	Repo     string
	HeadSeq  int64 // the AUDITHEAD item (0: none)
	HeadHash core.Hash
	// Last is the record new writes must follow: the head's record if it is
	// present, extended over records above it that link on, or else the
	// newest record below the head.
	LastSeq  int64
	LastHash core.Hash
	// Orphans are records present but not on the chain from Last.
	Orphans []int64
	// Lost are seqs up to the head that this region never received.
	Lost []int64
}

// Consistent reports whether the head and the records agree.
func (s ChainState) Consistent() bool {
	return s.HeadSeq == s.LastSeq && s.HeadHash == s.LastHash && len(s.Orphans) == 0
}

// DRChain is the ledger of writer changes (switchovers, promotions, drills).
const DRChain = "strata-dr"

// --- DynamoDB ---

const writerPK, writerSK = "SYSTEM", "WRITER"

func (d *Dynamo) fence() *types.TransactWriteItem {
	if d.Region == "" {
		return nil
	}
	cond := "#r = :me"
	if d.Region == d.primary() {
		cond = "attribute_not_exists(PK) OR #r = :me"
	}
	return &types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{
		TableName:                 &d.Table,
		Key:                       map[string]types.AttributeValue{"PK": s(writerPK), "SK": s(writerSK)},
		ConditionExpression:       aws.String(cond),
		ExpressionAttributeNames:  map[string]string{"#r": "region"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":me": s(d.Region)},
	}}
}

func (d *Dynamo) primary() string {
	if d.Primary != "" {
		return d.Primary
	}
	return d.Region
}

func (d *Dynamo) notWriter(ctx context.Context) error {
	w, err := d.Writer(ctx)
	if err != nil {
		return fmt.Errorf("%w (and reading the writer failed: %v)", ErrNotWriter, err)
	}
	return &NotWriterError{Here: d.Region, Writer: w}
}

// Writer implements Fence.
func (d *Dynamo) Writer(ctx context.Context) (Writer, error) {
	out, err := d.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &d.Table, ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{"PK": s(writerPK), "SK": s(writerSK)},
	})
	if err != nil {
		return Writer{}, err
	}
	if out.Item == nil {
		return Writer{Region: d.primary()}, nil
	}
	return decode[Writer](out.Item)
}

// SetWriter implements Fence.
func (d *Dynamo) SetWriter(ctx context.Context, w Writer, expectEpoch int64) error {
	it, err := item(writerPK, writerSK, w, map[string]types.AttributeValue{
		"region": s(w.Region), "epoch": &types.AttributeValueMemberN{Value: strconv.FormatInt(w.Epoch, 10)},
	})
	if err != nil {
		return err
	}
	in := &dynamodb.PutItemInput{TableName: &d.Table, Item: it}
	if expectEpoch == 0 {
		in.ConditionExpression = aws.String("attribute_not_exists(PK)")
	} else {
		in.ConditionExpression = aws.String("epoch = :e")
		in.ExpressionAttributeValues = map[string]types.AttributeValue{":e": &types.AttributeValueMemberN{Value: strconv.FormatInt(expectEpoch, 10)}}
	}
	_, err = d.Client.PutItem(ctx, in)
	if isConditionFailed(err) {
		return ErrConflict
	}
	return err
}

// Writable implements Fence.
func (d *Dynamo) Writable(ctx context.Context) error {
	if d.Region == "" {
		return nil
	}
	w, err := d.Writer(ctx)
	if err != nil {
		return err
	}
	if w.Region != d.Region {
		return &NotWriterError{Here: d.Region, Writer: w}
	}
	return nil
}

// AppendAudit implements Fence.
func (d *Dynamo) AppendAudit(ctx context.Context, repo string, audit core.AuditEntry) error {
	return d.transact(ctx, repo, audit, nil)
}

// InspectChain implements Fence.
func (d *Dynamo) InspectChain(ctx context.Context, repo string) (ChainState, error) {
	st := ChainState{Repo: repo}
	var err error
	if st.HeadSeq, st.HeadHash, err = d.auditHead(ctx, repo); err != nil {
		return st, err
	}
	// Records near the head: the last few below it and anything above.
	from := max(st.HeadSeq-inspectWindow, 0)
	recs, err := d.ListAudit(ctx, repo, from, 1000)
	if err != nil {
		return st, err
	}
	return inspect(st, recs, from)
}

// SetAuditHead implements Fence.
func (d *Dynamo) SetAuditHead(ctx context.Context, repo string, seq int64, hash core.Hash, expectSeq int64) error {
	head := map[string]types.AttributeValue{
		"PK": s(repoPK(repo)), "SK": s("AUDITHEAD"),
		"seq": &types.AttributeValueMemberN{Value: strconv.FormatInt(seq, 10)}, "hash": s(string(hash)),
	}
	in := &dynamodb.PutItemInput{TableName: &d.Table, Item: head}
	if expectSeq == 0 {
		in.ConditionExpression = aws.String("attribute_not_exists(PK)")
	} else {
		in.ConditionExpression = aws.String("seq = :s")
		in.ExpressionAttributeValues = map[string]types.AttributeValue{":s": &types.AttributeValueMemberN{Value: strconv.FormatInt(expectSeq, 10)}}
	}
	if seq == 0 {
		_, err := d.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &d.Table,
			Key: map[string]types.AttributeValue{"PK": s(repoPK(repo)), "SK": s("AUDITHEAD")}})
		return err
	}
	_, err := d.Client.PutItem(ctx, in)
	if isConditionFailed(err) {
		return ErrConflict
	}
	return err
}

// QuarantineAudit implements Fence.
func (d *Dynamo) QuarantineAudit(ctx context.Context, repo string, seqs []int64, epoch int64) error {
	for _, seq := range seqs {
		it, err := d.get(ctx, "AUDIT#"+repo, auditSK(seq))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		moved := maps.Clone(it)
		moved["PK"], moved["SK"] = s("ORPHAN#"+repo), s(fmt.Sprintf("%d#%s", epoch, auditSK(seq)))
		_, err = d.Client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{TableName: &d.Table, Item: moved}},
			{Delete: &types.Delete{TableName: &d.Table, Key: map[string]types.AttributeValue{"PK": it["PK"], "SK": it["SK"]}}},
		}})
		if err != nil {
			return err
		}
	}
	return nil
}

// inspectWindow is how far below the head promotion looks for the last
// record. Partly replicated transactions are at most seconds old.
const inspectWindow = 50

// inspect works out ChainState from the records after seq from, in order.
// The chain continues from the highest record reached from the window's
// first record without a gap or a broken link; records above it are orphans.
func inspect(st ChainState, recs []core.AuditRecord, from int64) (ChainState, error) {
	present := map[int64]core.AuditRecord{}
	for _, r := range recs {
		present[r.Seq] = r
	}
	var prev core.Hash
	if from > 0 {
		first, ok := present[from+1]
		if !ok && st.HeadSeq > from {
			return st, fmt.Errorf("chain %s: record %d (%d below head %d) is missing; repair by hand", st.Repo, from+1, inspectWindow, st.HeadSeq)
		}
		prev = first.Prev
	}
	st.LastSeq, st.LastHash = from, prev
	for {
		n, ok := present[st.LastSeq+1]
		if !ok || n.Prev != st.LastHash {
			break
		}
		st.LastSeq, st.LastHash = n.Seq, n.Hash
	}
	for _, r := range recs {
		if r.Seq > st.LastSeq {
			st.Orphans = append(st.Orphans, r.Seq)
		}
	}
	for s := st.LastSeq + 1; s <= st.HeadSeq; s++ {
		if _, ok := present[s]; !ok {
			st.Lost = append(st.Lost, s)
		}
	}
	return st, nil
}
