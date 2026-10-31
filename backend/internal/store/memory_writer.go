package store

import (
	"context"
	"maps"
	"slices"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

type memHead struct {
	seq  int64
	hash core.Hash
}

func (m *Memory) primary() string {
	if m.Primary != "" {
		return m.Primary
	}
	return m.Region
}

func (m *Memory) checkFence() error {
	if m.Region == "" {
		return nil
	}
	if m.writer == nil && m.Region == m.primary() {
		return nil
	}
	if m.writer != nil && m.writer.Region == m.Region {
		return nil
	}
	w := Writer{Region: m.primary()}
	if m.writer != nil {
		w = *m.writer
	}
	return &NotWriterError{Here: m.Region, Writer: w}
}

// Writer implements Fence.
func (m *Memory) Writer(context.Context) (Writer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writer == nil {
		return Writer{Region: m.primary()}, nil
	}
	return *m.writer, nil
}

// SetWriter implements Fence.
func (m *Memory) SetWriter(_ context.Context, w Writer, expectEpoch int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := int64(0)
	if m.writer != nil {
		cur = m.writer.Epoch
	}
	if (m.writer == nil) != (expectEpoch == 0) || cur != expectEpoch {
		return ErrConflict
	}
	m.writer = &w
	return nil
}

// Writable implements Fence.
func (m *Memory) Writable(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkFence()
}

// AppendAudit implements Fence.
func (m *Memory) AppendAudit(_ context.Context, repo string, audit core.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, err := m.prepareAudit(repo, audit)
	if err != nil {
		return err
	}
	m.commitAudit(rec)
	return nil
}

// InspectChain implements Fence.
func (m *Memory) InspectChain(_ context.Context, repo string) (ChainState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := ChainState{Repo: repo}
	st.HeadSeq, st.HeadHash = m.head(repo)
	from := max(st.HeadSeq-inspectWindow, 0)
	var recs []core.AuditRecord
	for _, r := range m.audits[repo] {
		if r.Seq > from {
			recs = append(recs, r)
		}
	}
	return inspect(st, recs, from)
}

// SetAuditHead implements Fence.
func (m *Memory) SetAuditHead(_ context.Context, repo string, seq int64, hash core.Hash, expectSeq int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, _ := m.head(repo); cur != expectSeq {
		return ErrConflict
	}
	if m.heads == nil {
		m.heads = map[string]memHead{}
	}
	m.heads[repo] = memHead{seq, hash}
	return nil
}

// QuarantineAudit implements Fence.
func (m *Memory) QuarantineAudit(_ context.Context, repo string, seqs []int64, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keep []core.AuditRecord
	for _, r := range m.audits[repo] {
		if slices.Contains(seqs, r.Seq) {
			m.Orphans = append(m.Orphans, r)
			continue
		}
		keep = append(keep, r)
	}
	m.audits[repo] = keep
	return nil
}

// SimulateLostRecords drops a chain's last n records but keeps its head, as
// a standby holds a transaction whose head update arrived but its record did
// not (tests).
func (m *Memory) SimulateLostRecords(repo string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seq, h := m.head(repo)
	a := m.audits[repo]
	m.audits[repo] = slices.Clone(a[:len(a)-n])
	if m.heads == nil {
		m.heads = map[string]memHead{}
	}
	m.heads[repo] = memHead{seq, h}
}

// SimulateHeadBehind moves a chain's head back n records, as a standby holds
// records whose head update has not arrived (tests).
func (m *Memory) SimulateHeadBehind(repo string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.audits[repo]
	if m.heads == nil {
		m.heads = map[string]memHead{}
	}
	if len(a) == n {
		m.heads[repo] = memHead{}
		return
	}
	r := a[len(a)-1-n]
	m.heads[repo] = memHead{r.Seq, r.Hash}
}

// ReplicateTo copies this store's replicated state into dst, as a global
// table and bucket replication would once caught up: index, ledger, writer,
// objects (tests). dst keeps its own Region, Primary and archive progress.
func (m *Memory) ReplicateTo(dst *Memory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dst.mu.Lock()
	defer dst.mu.Unlock()
	dst.objects, dst.changeDocs = maps.Clone(m.objects), maps.Clone(m.changeDocs)
	dst.repos, dst.changes, dst.pointers = maps.Clone(m.repos), maps.Clone(m.changes), maps.Clone(m.pointers)
	dst.refs = map[string]map[string]core.Ref{}
	for k, v := range m.refs {
		dst.refs[k] = maps.Clone(v)
	}
	dst.annotations, dst.approvals = cloneSlices(m.annotations), cloneSlices(m.approvals)
	dst.grants, dst.activeGrant = maps.Clone(m.grants), maps.Clone(m.activeGrant)
	dst.grantActivity, dst.index, dst.audits = cloneSlices(m.grantActivity), cloneSlices(m.index), cloneSlices(m.audits)
	dst.heads = maps.Clone(m.heads)
	if m.writer != nil {
		w := *m.writer
		dst.writer = &w
	}
	if m.anchors != nil {
		a := *m.anchors
		a.list = slices.Clone(a.list)
		a.proofs = cloneSlices(a.proofs)
		if dst.anchors != nil {
			a.progress = dst.anchors.progress
		} else {
			a.progress = map[string]int64{}
		}
		dst.anchors = &a
	}
}

func cloneSlices[T any](m map[string][]T) map[string][]T {
	out := make(map[string][]T, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}

// ObjectKeys lists stored objects and change documents, like a bucket listing.
func (m *Memory) ObjectKeys(context.Context) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for k := range m.objects {
		out["o/"+k] = true
	}
	for k := range m.changeDocs {
		out["c/"+k] = true
	}
	return out, nil
}

// ReplayRecord puts a record back into its chain without moving the head,
// as a late replicated item arrives (tests).
func (m *Memory) ReplayRecord(r core.AuditRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := append(m.audits[r.Repo], r)
	slices.SortFunc(a, func(x, y core.AuditRecord) int { return int(x.Seq - y.Seq) })
	m.audits[r.Repo] = a
}
