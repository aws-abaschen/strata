package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Dynamo is the index, in one table (PK, SK) with one GSI (GSI1PK, GSI1SK):
//
//	PK=REPO#<repo>           SK=META                    repository (GSI1PK=REPOS)
//	PK=REPO#<repo>           SK=REF#<name>              ref, CAS on target
//	PK=REPO#<repo>           SK=REV#<rev>               change revision
//	PK=REPO#<repo>           SK=CHG#<changeId>          latest revision of a change id, CAS
//	PK=REPO#<repo>           SK=NOTE#<rev>#<time>#<id>  annotation
//	PK=WI#<system>:<key>     SK=<time>#<repo>#<rev>#<via>  work-item index
//	PK=LBL#<label>           SK=<time>#<repo>#<rev>#<via>  label index
//
// Every item stores its JSON document in the "doc" attribute.
type Dynamo struct {
	Client *dynamodb.Client
	Table  string
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func repoPK(repo string) string { return "REPO#" + repo }

func item(pk, sk string, doc any, extra map[string]types.AttributeValue) (map[string]types.AttributeValue, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	it := map[string]types.AttributeValue{"PK": s(pk), "SK": s(sk), "doc": s(string(b))}
	for k, v := range extra {
		it[k] = v
	}
	return it, nil
}

func decode[T any](it map[string]types.AttributeValue) (T, error) {
	var v T
	d, ok := it["doc"].(*types.AttributeValueMemberS)
	if !ok {
		return v, errors.New("item without doc")
	}
	return v, json.Unmarshal([]byte(d.Value), &v)
}

func isConditionFailed(err error) bool {
	var ccf *types.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

func (d *Dynamo) get(ctx context.Context, pk, sk string) (map[string]types.AttributeValue, error) {
	out, err := d.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &d.Table, Key: map[string]types.AttributeValue{"PK": s(pk), "SK": s(sk)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, ErrNotFound
	}
	return out.Item, nil
}

func (d *Dynamo) query(ctx context.Context, in *dynamodb.QueryInput, limit int) ([]map[string]types.AttributeValue, error) {
	in.TableName = &d.Table
	var items []map[string]types.AttributeValue
	p := dynamodb.NewQueryPaginator(d.Client, in)
	for p.HasMorePages() && (limit <= 0 || len(items) < limit) {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func extendsKey(l *core.LayerRef) string {
	if l == nil {
		return ""
	}
	return l.String()
}

func repoItem(r core.Repo) (map[string]types.AttributeValue, error) {
	return item(repoPK(r.Name), "META", r, map[string]types.AttributeValue{
		"GSI1PK": s("REPOS"), "GSI1SK": s(r.Name), "extends": s(extendsKey(r.Extends)),
	})
}

// subscriber index: PK=SUBS#<layer> SK=REPO#<repo>
func subsKey(layer, repo string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"PK": s("SUBS#" + layer), "SK": s("REPO#" + repo)}
}

// CreateRepo implements Meta.
func (d *Dynamo) CreateRepo(ctx context.Context, r core.Repo, audit core.AuditEntry) error {
	it, err := repoItem(r)
	if err != nil {
		return err
	}
	tx := []types.TransactWriteItem{{Put: &types.Put{Item: it, ConditionExpression: aws.String("attribute_not_exists(PK)")}}}
	if r.Extends != nil {
		sub, err := item("SUBS#"+r.Extends.Layer, "REPO#"+r.Name, r, nil)
		if err != nil {
			return err
		}
		tx = append(tx, types.TransactWriteItem{Put: &types.Put{Item: sub}})
	}
	return d.transact(ctx, r.Name, audit, tx)
}

// UpdateRepo implements Meta.
func (d *Dynamo) UpdateRepo(ctx context.Context, r core.Repo, previous *core.LayerRef, audit core.AuditEntry) error {
	it, err := repoItem(r)
	if err != nil {
		return err
	}
	// DynamoDB allows one operation per item per transaction, so existence and
	// the CAS on the old Layer are one condition on the single Put. A repo
	// that is gone is reported before the transaction.
	if _, err := d.get(ctx, repoPK(r.Name), "META"); err != nil {
		return err
	}
	tx := []types.TransactWriteItem{
		{Put: &types.Put{Item: it, ConditionExpression: aws.String("attribute_exists(PK) AND extends = :prev"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":prev": s(extendsKey(previous))}}},
	}
	if previous != nil && (r.Extends == nil || previous.Layer != r.Extends.Layer) {
		tx = append(tx, types.TransactWriteItem{Delete: &types.Delete{Key: subsKey(previous.Layer, r.Name)}})
	}
	if r.Extends != nil {
		sub, err := item("SUBS#"+r.Extends.Layer, "REPO#"+r.Name, r, nil)
		if err != nil {
			return err
		}
		tx = append(tx, types.TransactWriteItem{Put: &types.Put{Item: sub}})
	}
	err = d.transact(ctx, r.Name, audit, tx)
	if errors.Is(err, ErrExists) { // item 0 failed: the Layer changed since it was read
		return ErrConflict
	}
	return err
}

// ListSubscribers implements Meta.
func (d *Dynamo) ListSubscribers(ctx context.Context, layer string) ([]core.Repo, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :p"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("SUBS#" + layer)},
	}, 0)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.Repo](items)
}

// GetRepo implements Meta.
func (d *Dynamo) GetRepo(ctx context.Context, name string) (core.Repo, error) {
	it, err := d.get(ctx, repoPK(name), "META")
	if err != nil {
		return core.Repo{}, err
	}
	return decode[core.Repo](it)
}

// ListRepos implements Meta.
func (d *Dynamo) ListRepos(ctx context.Context) ([]core.Repo, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		IndexName: aws.String("GSI1"), KeyConditionExpression: aws.String("GSI1PK = :p"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("REPOS")},
	}, 0)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.Repo](items)
}

func decodeAll[T any](items []map[string]types.AttributeValue) ([]T, error) {
	out := make([]T, 0, len(items))
	for _, it := range items {
		v, err := decode[T](it)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ListRefs implements Meta.
func (d *Dynamo) ListRefs(ctx context.Context, repo string) ([]core.Ref, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :p AND begins_with(SK, :s)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s(repoPK(repo)), ":s": s("REF#")},
	}, 0)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.Ref](items)
}

// GetRef implements Meta.
func (d *Dynamo) GetRef(ctx context.Context, repo, name string) (core.Ref, error) {
	it, err := d.get(ctx, repoPK(repo), "REF#"+name)
	if err != nil {
		return core.Ref{}, err
	}
	return decode[core.Ref](it)
}

// refCondition is the CAS check for a ref item.
func refCondition(expected core.Hash) (*string, map[string]types.AttributeValue) {
	if expected == "" {
		return aws.String("attribute_not_exists(PK)"), nil
	}
	return aws.String("target = :exp"), map[string]types.AttributeValue{":exp": s(string(expected))}
}

// UpdateRef implements Meta.
func (d *Dynamo) UpdateRef(ctx context.Context, repo string, ref core.Ref, expected core.Hash, audit core.AuditEntry) error {
	cond, vals := refCondition(expected)
	var w types.TransactWriteItem
	if ref.Target == "" {
		w.Delete = &types.Delete{Key: map[string]types.AttributeValue{"PK": s(repoPK(repo)), "SK": s("REF#" + ref.Name)},
			ConditionExpression: cond, ExpressionAttributeValues: vals}
	} else {
		it, err := item(repoPK(repo), "REF#"+ref.Name, ref, map[string]types.AttributeValue{"target": s(string(ref.Target))})
		if err != nil {
			return err
		}
		w.Put = &types.Put{Item: it, ConditionExpression: cond, ExpressionAttributeValues: vals}
	}
	err := d.transact(ctx, repo, audit, []types.TransactWriteItem{w})
	if errors.Is(err, ErrExists) { // the only item is the CAS
		return ErrConflict
	}
	return err
}

func indexItems(md core.Metadata, sum core.ChangeSummary) ([]types.TransactWriteItem, error) {
	sk := sum.CreatedAt.UTC().Format(time.RFC3339Nano) + "#" + sum.Repo + "#" + string(sum.Revision) + "#" + sum.Via
	var pks []string
	for _, w := range md.WorkItems {
		pks = append(pks, "WI#"+w.String())
	}
	for _, l := range md.Labels {
		pks = append(pks, "LBL#"+l)
	}
	var out []types.TransactWriteItem
	for _, pk := range pks {
		it, err := item(pk, sk, sum, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, types.TransactWriteItem{Put: &types.Put{TableName: nil, Item: it}})
	}
	return out, nil
}

// PutChange implements Meta.
func (d *Dynamo) PutChange(ctx context.Context, c core.Change, ref *RefUpdate, audit core.AuditEntry) error {
	pk := repoPK(c.Repo)
	rev, err := item(pk, "REV#"+string(c.Revision), c, nil)
	if err != nil {
		return err
	}
	tx := []types.TransactWriteItem{{Put: &types.Put{Item: rev, ConditionExpression: aws.String("attribute_not_exists(PK)")}}}

	ptr := map[string]types.AttributeValue{"PK": s(pk), "SK": s("CHG#" + c.ChangeID), "latest": s(string(c.Revision))}
	if c.Predecessor == "" {
		tx = append(tx, types.TransactWriteItem{Put: &types.Put{Item: ptr, ConditionExpression: aws.String("attribute_not_exists(PK)")}})
	} else {
		tx = append(tx, types.TransactWriteItem{Put: &types.Put{Item: ptr, ConditionExpression: aws.String("latest = :pred"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pred": s(string(c.Predecessor))}}})
	}

	if ref != nil {
		r := core.Ref{Name: ref.Name, Target: c.Revision, UpdatedBy: c.Author, UpdatedAt: c.CreatedAt}
		it, err := item(pk, "REF#"+ref.Name, r, map[string]types.AttributeValue{"target": s(string(c.Revision))})
		if err != nil {
			return err
		}
		cond, vals := refCondition(ref.Expected)
		tx = append(tx, types.TransactWriteItem{Put: &types.Put{Item: it, ConditionExpression: cond, ExpressionAttributeValues: vals}})
	}

	idx, err := indexItems(c.Metadata, c.Summary())
	if err != nil {
		return err
	}
	return d.transact(ctx, c.Repo, audit, append(tx, idx...))
}

// Audit ledger items:
//
//	PK=REPO#<repo>   SK=AUDITHEAD   seq and hash of the last record (CAS)
//	PK=AUDIT#<repo>  SK=<seq, 12 digits>  the record (streamed to the ledger Lambda)
func auditSK(seq int64) string { return fmt.Sprintf("%012d", seq) }

func (d *Dynamo) auditHead(ctx context.Context, repo string) (int64, core.Hash, error) {
	it, err := d.get(ctx, repoPK(repo), "AUDITHEAD")
	if errors.Is(err, ErrNotFound) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	n, _ := it["seq"].(*types.AttributeValueMemberN)
	h, _ := it["hash"].(*types.AttributeValueMemberS)
	if n == nil || h == nil {
		return 0, "", errors.New("corrupt audit head")
	}
	seq, err := strconv.ParseInt(n.Value, 10, 64)
	return seq, core.Hash(h.Value), err
}

// transact writes tx and the repository's next audit record atomically.
// Condition failures map to ErrExists (item 0) or ErrConflict; losing the
// race for the audit head retries with the new head.
func (d *Dynamo) transact(ctx context.Context, repo string, audit core.AuditEntry, tx []types.TransactWriteItem) error {
	for attempt := 0; attempt < 8; attempt++ {
		seq, prev, err := d.auditHead(ctx, repo)
		if err != nil {
			return err
		}
		rec, err := core.NextAuditRecord(repo, seq, prev, audit)
		if err != nil {
			return err
		}
		recItem, err := item("AUDIT#"+repo, auditSK(rec.Seq), rec, map[string]types.AttributeValue{"action": s(rec.Action)})
		if err != nil {
			return err
		}
		head := map[string]types.AttributeValue{
			"PK": s(repoPK(repo)), "SK": s("AUDITHEAD"),
			"seq": &types.AttributeValueMemberN{Value: strconv.FormatInt(rec.Seq, 10)}, "hash": s(string(rec.Hash)),
		}
		headPut := &types.Put{Item: head, ConditionExpression: aws.String("attribute_not_exists(PK)")}
		if seq > 0 {
			headPut = &types.Put{Item: head, ConditionExpression: aws.String("seq = :s"),
				ExpressionAttributeValues: map[string]types.AttributeValue{":s": &types.AttributeValueMemberN{Value: strconv.FormatInt(seq, 10)}}}
		}
		all := append(slices.Clone(tx),
			types.TransactWriteItem{Put: &types.Put{Item: recItem, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
			types.TransactWriteItem{Put: headPut})
		if audit.BreakGlass != "" {
			// What a grant was used for, readable without scanning every ledger.
			act, err := item("BGACT#"+audit.BreakGlass, repo+"#"+auditSK(rec.Seq), rec, nil)
			if err != nil {
				return err
			}
			all = append(all, types.TransactWriteItem{Put: &types.Put{Item: act}})
		}
		for _, t := range all {
			switch {
			case t.Put != nil:
				t.Put.TableName = &d.Table
			case t.ConditionCheck != nil:
				t.ConditionCheck.TableName = &d.Table
			case t.Delete != nil:
				t.Delete.TableName = &d.Table
			}
		}
		_, err = d.Client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: all})
		var tce *types.TransactionCanceledException
		if !errors.As(err, &tce) {
			return err
		}
		for i, r := range tce.CancellationReasons {
			if aws.ToString(r.Code) != "ConditionalCheckFailed" || i >= len(tx) {
				continue
			}
			if i == 0 {
				return ErrExists
			}
			return ErrConflict
		}
		// Only the audit head moved (or a transient conflict): retry.
	}
	return fmt.Errorf("%w: audit ledger busy, retry", ErrConflict)
}

// ListAudit implements Meta.
func (d *Dynamo) ListAudit(ctx context.Context, repo string, after int64, limit int) ([]core.AuditRecord, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :p AND SK > :s"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("AUDIT#" + repo), ":s": s(auditSK(after))},
		ConsistentRead:            aws.Bool(true),
	}, limit)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.AuditRecord](items)
}

// GetChange implements Meta.
func (d *Dynamo) GetChange(ctx context.Context, repo string, rev core.Hash) (core.Change, error) {
	it, err := d.get(ctx, repoPK(repo), "REV#"+string(rev))
	if err != nil {
		return core.Change{}, err
	}
	return decode[core.Change](it)
}

// LatestRevision implements Meta.
func (d *Dynamo) LatestRevision(ctx context.Context, repo, changeID string) (core.Hash, error) {
	it, err := d.get(ctx, repoPK(repo), "CHG#"+changeID)
	if err != nil {
		return "", err
	}
	v, _ := it["latest"].(*types.AttributeValueMemberS)
	if v == nil {
		return "", ErrNotFound
	}
	return core.Hash(v.Value), nil
}

// PutAnnotation implements Meta.
func (d *Dynamo) PutAnnotation(ctx context.Context, a core.Annotation, audit core.AuditEntry) error {
	c, err := d.GetChange(ctx, a.Repo, a.Revision)
	if err != nil {
		return err
	}
	var rnd [4]byte
	rand.Read(rnd[:])
	sk := "NOTE#" + string(a.Revision) + "#" + a.CreatedAt.UTC().Format(time.RFC3339Nano) + "#" + hex.EncodeToString(rnd[:])
	it, err := item(repoPK(a.Repo), sk, a, nil)
	if err != nil {
		return err
	}
	sum := c.Summary()
	sum.Via = "annotation"
	idx, err := indexItems(a.Metadata, sum)
	if err != nil {
		return err
	}
	tx := append([]types.TransactWriteItem{{Put: &types.Put{Item: it, ConditionExpression: aws.String("attribute_not_exists(PK)")}}}, idx...)
	return d.transact(ctx, a.Repo, audit, tx)
}

// ListAnnotations implements Meta.
func (d *Dynamo) ListAnnotations(ctx context.Context, repo string, rev core.Hash) ([]core.Annotation, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :p AND begins_with(SK, :s)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s(repoPK(repo)), ":s": s("NOTE#" + string(rev) + "#")},
	}, 0)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.Annotation](items)
}

// PutApproval implements Meta.
func (d *Dynamo) PutApproval(ctx context.Context, a core.Approval, audit core.AuditEntry) error {
	if _, err := d.GetChange(ctx, a.Repo, a.Revision); err != nil {
		return err
	}
	it, err := item(repoPK(a.Repo), "APPR#"+string(a.Revision)+"#"+a.Approver, a, nil)
	if err != nil {
		return err
	}
	return d.transact(ctx, a.Repo, audit, []types.TransactWriteItem{
		{Put: &types.Put{Item: it, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
	})
}

// ListApprovals implements Meta.
func (d *Dynamo) ListApprovals(ctx context.Context, repo string, rev core.Hash) ([]core.Approval, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :p AND begins_with(SK, :s)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s(repoPK(repo)), ":s": s("APPR#" + string(rev) + "#")},
		ConsistentRead:            aws.Bool(true),
	}, 0)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.Approval](items)
}

func (d *Dynamo) find(ctx context.Context, pk string, limit int) ([]core.ChangeSummary, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :p"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s(pk)},
		ScanIndexForward:          aws.Bool(false),
		Limit:                     aws.Int32(int32(limit)),
	}, limit)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.ChangeSummary](items)
}

// FindByWorkItem implements Meta.
func (d *Dynamo) FindByWorkItem(ctx context.Context, system, key string, limit int) ([]core.ChangeSummary, error) {
	return d.find(ctx, "WI#"+core.WorkItem{System: system, Key: key}.String(), limit)
}

// FindByLabel implements Meta.
func (d *Dynamo) FindByLabel(ctx context.Context, label string, limit int) ([]core.ChangeSummary, error) {
	return d.find(ctx, "LBL#"+label, limit)
}
