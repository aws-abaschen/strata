package store

import (
	"context"
	"errors"
	"maps"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Anchors stores signed chain-head anchors (see core.Anchor) and the
// reconciler's progress.
type Anchors interface {
	// AuditHead returns the last record of a chain (0, "" if empty).
	AuditHead(ctx context.Context, repo string) (int64, core.Hash, error)
	// LatestAnchor returns the newest anchor and whether its per-repository
	// proofs are indexed (ErrNotFound if there is none).
	LatestAnchor(ctx context.Context) (core.Anchor, bool, error)
	// PutAnchor stores a as the next anchor: a.Statement.Seq must be the
	// latest seq + 1 (ErrConflict otherwise), so anchors never fork.
	PutAnchor(ctx context.Context, a core.Anchor) error
	// PutAnchorProofs indexes proofs for anchor seq, then marks it indexed.
	// Idempotent.
	PutAnchorProofs(ctx context.Context, seq int64, proofs []core.AnchorProof) error
	// ListAnchorProofs returns a chain's proofs under anchors after the given
	// anchor seq, oldest first, at most limit.
	ListAnchorProofs(ctx context.Context, repo string, after int64, limit int) ([]core.AnchorProof, error)
	// AnchorProgress is, per chain, the seq up to which the Object Lock
	// archive was checked against the table.
	AnchorProgress(ctx context.Context) (map[string]int64, error)
	SetAnchorProgress(ctx context.Context, p map[string]int64) error
}

// Anchor items:
//
//	PK=ANCHOR SK=<seq, 12 digits>     the full anchor, written once
//	PK=ANCHOR SK=LATEST               copy of the newest anchor; seq (CAS), indexed
//	PK=ANCHOR SK=PROGRESS             reconciler progress
//	PK=ANCHORED#<repo> SK=<seq>       that chain's proof under anchor seq
const anchorPK = "ANCHOR"

// AuditHead implements Anchors.
func (d *Dynamo) AuditHead(ctx context.Context, repo string) (int64, core.Hash, error) {
	return d.auditHead(ctx, repo)
}

// LatestAnchor implements Anchors.
func (d *Dynamo) LatestAnchor(ctx context.Context) (core.Anchor, bool, error) {
	it, err := d.get(ctx, anchorPK, "LATEST")
	if err != nil {
		return core.Anchor{}, false, err
	}
	a, err := decode[core.Anchor](it)
	_, indexed := it["indexed"]
	return a, indexed, err
}

// PutAnchor implements Anchors.
func (d *Dynamo) PutAnchor(ctx context.Context, a core.Anchor) error {
	seq := a.Statement.Seq
	full, err := item(anchorPK, auditSK(seq), a, nil)
	if err != nil {
		return err
	}
	latest, err := item(anchorPK, "LATEST", a, map[string]types.AttributeValue{"seq": ms(seq)})
	if err != nil {
		return err
	}
	latestPut := &types.Put{TableName: &d.Table, Item: latest, ConditionExpression: aws.String("attribute_not_exists(PK)")}
	if seq > 1 {
		latestPut.ConditionExpression = aws.String("seq = :p")
		latestPut.ExpressionAttributeValues = map[string]types.AttributeValue{":p": ms(seq - 1)}
	}
	_, err = d.Client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: &d.Table, Item: full, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
		{Put: latestPut},
	}})
	var tce *types.TransactionCanceledException
	if errors.As(err, &tce) {
		return ErrConflict
	}
	return err
}

// PutAnchorProofs implements Anchors.
func (d *Dynamo) PutAnchorProofs(ctx context.Context, seq int64, proofs []core.AnchorProof) error {
	for i := 0; i < len(proofs); i += 25 {
		var reqs []types.WriteRequest
		for _, p := range proofs[i:min(i+25, len(proofs))] {
			it, err := item("ANCHORED#"+p.Leaf.Repo, auditSK(seq), p, nil)
			if err != nil {
				return err
			}
			reqs = append(reqs, types.WriteRequest{PutRequest: &types.PutRequest{Item: it}})
		}
		for len(reqs) > 0 {
			out, err := d.Client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{d.Table: reqs}})
			if err != nil {
				return err
			}
			reqs = out.UnprocessedItems[d.Table]
		}
	}
	_, err := d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &d.Table,
		Key:                       map[string]types.AttributeValue{"PK": s(anchorPK), "SK": s("LATEST")},
		UpdateExpression:          aws.String("SET #i = :t"),
		ExpressionAttributeNames:  map[string]string{"#i": "indexed"},
		ConditionExpression:       aws.String("seq = :s"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":t": s("1"), ":s": ms(seq)},
	})
	if isConditionFailed(err) {
		return nil // a newer anchor exists; this one's proofs are written
	}
	return err
}

// ListAnchorProofs implements Anchors.
func (d *Dynamo) ListAnchorProofs(ctx context.Context, repo string, after int64, limit int) ([]core.AnchorProof, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :p AND SK > :s"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("ANCHORED#" + repo), ":s": s(auditSK(after))},
		Limit:                     aws.Int32(int32(limit)),
	}, limit)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.AnchorProof](items)
}

// AnchorProgress implements Anchors.
func (d *Dynamo) AnchorProgress(ctx context.Context) (map[string]int64, error) {
	it, err := d.get(ctx, anchorPK, "PROGRESS")
	if errors.Is(err, ErrNotFound) {
		return map[string]int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decode[map[string]int64](it)
}

// SetAnchorProgress implements Anchors.
func (d *Dynamo) SetAnchorProgress(ctx context.Context, p map[string]int64) error {
	it, err := item(anchorPK, "PROGRESS", p, nil)
	if err != nil {
		return err
	}
	_, err = d.Client.PutItem(ctx, &dynamodb.PutItemInput{TableName: &d.Table, Item: it})
	return err
}

// Memory implementation.

type memAnchors struct {
	list     []core.Anchor
	indexed  bool
	proofs   map[string][]core.AnchorProof
	progress map[string]int64
}

func (m *Memory) anc() *memAnchors {
	if m.anchors == nil {
		m.anchors = &memAnchors{proofs: map[string][]core.AnchorProof{}, progress: map[string]int64{}}
	}
	return m.anchors
}

// AuditHead implements Anchors.
func (m *Memory) AuditHead(_ context.Context, repo string) (int64, core.Hash, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.audits[repo]; len(a) > 0 {
		return a[len(a)-1].Seq, a[len(a)-1].Hash, nil
	}
	return 0, "", nil
}

// LatestAnchor implements Anchors.
func (m *Memory) LatestAnchor(context.Context) (core.Anchor, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.anc()
	if len(a.list) == 0 {
		return core.Anchor{}, false, ErrNotFound
	}
	return a.list[len(a.list)-1], a.indexed, nil
}

// PutAnchor implements Anchors.
func (m *Memory) PutAnchor(_ context.Context, x core.Anchor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.anc()
	if x.Statement.Seq != int64(len(a.list))+1 {
		return ErrConflict
	}
	a.list, a.indexed = append(a.list, x), false
	return nil
}

// PutAnchorProofs implements Anchors.
func (m *Memory) PutAnchorProofs(_ context.Context, seq int64, proofs []core.AnchorProof) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.anc()
	for _, p := range proofs {
		ps := a.proofs[p.Leaf.Repo]
		if len(ps) == 0 || ps[len(ps)-1].Statement.Seq < seq {
			a.proofs[p.Leaf.Repo] = append(ps, p)
		}
	}
	if int64(len(a.list)) == seq {
		a.indexed = true
	}
	return nil
}

// ListAnchorProofs implements Anchors.
func (m *Memory) ListAnchorProofs(_ context.Context, repo string, after int64, limit int) ([]core.AnchorProof, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []core.AnchorProof{}
	for _, p := range m.anc().proofs[repo] {
		if p.Statement.Seq > after && len(out) < limit {
			out = append(out, p)
		}
	}
	return out, nil
}

// DropAnchorProofs deletes a chain's proofs (tests only: what an operator
// with table access could do to hide a rewrite from readers).
func (m *Memory) DropAnchorProofs(repo string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.anc().proofs, repo)
}

// AnchorProgress implements Anchors.
func (m *Memory) AnchorProgress(context.Context) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.anc().progress), nil
}

// SetAnchorProgress implements Anchors.
func (m *Memory) SetAnchorProgress(_ context.Context, p map[string]int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.anc().progress = maps.Clone(p)
	return nil
}

// RewriteAuditTail replaces records from seq on with a freshly chained,
// different history (tests only: what an operator with table access could do).
func (m *Memory) RewriteAuditTail(repo string, seq int64, actor string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	recs := m.audits[repo]
	for i := range recs {
		if recs[i].Seq < seq {
			continue
		}
		e := core.AuditEntry{Action: recs[i].Action, Actor: actor, At: recs[i].At, Detail: recs[i].Detail}
		r, _ := core.NextAuditRecord(repo, recs[i].Seq-1, recs[i].Prev, e)
		if i > 0 {
			r, _ = core.NextAuditRecord(repo, recs[i-1].Seq, recs[i-1].Hash, e)
		}
		recs[i] = r
	}
}
