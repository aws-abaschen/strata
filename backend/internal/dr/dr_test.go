package dr_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/dr"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

const west, central = "eu-west-1", "eu-central-1"

func pair() (*store.Memory, *store.Memory, dr.Region, dr.Region) {
	a, b := store.NewMemory(), store.NewMemory()
	a.Region, a.Primary = west, west
	b.Region, b.Primary = central, west
	return a, b,
		dr.Region{Name: west, API: "https://west.example", Meta: a, Keys: a.ObjectKeys},
		dr.Region{Name: central, API: "https://central.example", Meta: b, Keys: b.ObjectKeys}
}

var op = dr.Op{Actor: "arn:aws:iam::1:role/ops", Reason: "quarterly DR drill", WorkItem: "OPS-7", Poll: time.Millisecond}

func entry(t *testing.T, action string) core.AuditEntry {
	t.Helper()
	e, err := core.NewAuditEntry(action, "ana@example.com", time.Now(), map[string]string{"repo": "payments"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func write(t *testing.T, m *store.Memory, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := m.GetRepo(ctx, "payments"); errors.Is(err, store.ErrNotFound) {
		if err := m.CreateRepo(ctx, core.Repo{Name: "payments", Kind: core.KindCode}, entry(t, "RepoCreated")); err != nil {
			t.Fatal(err)
		}
		n--
	}
	for range n {
		if err := m.AppendAudit(ctx, "payments", entry(t, "ChangeAnnotated")); err != nil {
			t.Fatal(err)
		}
	}
}

func verifyChain(t *testing.T, m *store.Memory, repo string) []core.AuditRecord {
	t.Helper()
	recs, _ := m.ListAudit(context.Background(), repo, 0, 1000)
	if _, err := core.VerifyAudit(recs, ""); err != nil {
		t.Fatalf("%s chain: %v", repo, err)
	}
	return recs
}

// The standby refuses writes and names the writer; so does a demoted primary.
func TestFence(t *testing.T) {
	a, b, _, _ := pair()
	write(t, a, 2)
	err := b.AppendAudit(context.Background(), "payments", entry(t, "ChangeAnnotated"))
	var nw *store.NotWriterError
	if !errors.As(err, &nw) || nw.Writer.Region != west {
		t.Fatalf("standby write: %v", err)
	}
	if err := a.SetWriter(context.Background(), store.Writer{Region: central, Epoch: 1}, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.AppendAudit(context.Background(), "payments", entry(t, "ChangeAnnotated")); !errors.Is(err, store.ErrNotWriter) {
		t.Fatalf("demoted primary write: %v", err)
	}
}

// Switchover waits for replication, loses nothing, and records itself.
func TestSwitchoverAndBack(t *testing.T) {
	a, b, west, central := pair()
	ctx := context.Background()
	if _, err := dr.Init(ctx, west, op); err != nil {
		t.Fatal(err)
	}
	write(t, a, 3)
	a.ReplicateTo(b) // steady state: the standby has caught up once
	write(t, a, 2)   // then lags
	a.Put("payments", []byte("an object"))

	// Replication lags: it only catches up while switchover is waiting.
	res, err := dr.Switchover(ctx, west, central, replicating(a, b), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Epoch != 3 || !res.OldFenced || res.WritesUnavailable == "" || res.Objects != 1 {
		t.Fatalf("result %+v", res)
	}
	// central writes now, west refuses.
	write(t, b, 2)
	if err := a.AppendAudit(ctx, "payments", entry(t, "ChangeAnnotated")); !errors.Is(err, store.ErrNotWriter) {
		t.Fatalf("old writer still writes: %v", err)
	}
	recs := verifyChain(t, b, "payments")
	if len(recs) != 7 {
		t.Fatalf("central has %d records, want 7", len(recs))
	}
	drRecs := verifyChain(t, b, store.DRChain)
	if last := drRecs[len(drRecs)-1]; last.Action != "WriterChanged" || !strings.Contains(string(last.Detail), `"mode":"switchover"`) {
		t.Fatalf("DR chain: %+v", last)
	}

	// And back: replication now runs the other way. Switchover refuses until
	// the old writer has seen the new writer record.
	if _, err := dr.Switchover(ctx, central, west, op, time.Second); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("switchback before replication: %v", err)
	}
	b.ReplicateTo(a)
	if _, err := dr.Switchover(ctx, central, west, replicating(b, a), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	write(t, a, 1)
	if n := len(verifyChain(t, a, "payments")); n != 8 {
		t.Fatalf("west has %d records after failback, want 8", n)
	}
}

// Switchover never proceeds while the target is behind.
func TestSwitchoverTimesOutWithoutReplication(t *testing.T) {
	a, _, west, central := pair()
	ctx := context.Background()
	dr.Init(ctx, west, op)
	write(t, a, 3)
	_, err := dr.Switchover(ctx, west, central, op, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "the regions disagree") {
		t.Fatalf("switchover with no replicated writer: %v", err)
	}
}

// A forced promotion repairs each way a transaction can be partly
// replicated, records what was lost, and the chains go on verifiably.
func TestPromoteRepairsPartlyReplicatedChains(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		damage   func(b *store.Memory)
		lost     int
		orphans  int
		recordsB int // records in central before the repair record
	}{
		{"consistent", func(*store.Memory) {}, 0, 0, 6},
		{"head arrived, records did not", func(b *store.Memory) { b.SimulateLostRecords("payments", 2) }, 2, 0, 4},
		{"records arrived, head did not", func(b *store.Memory) { b.SimulateHeadBehind("payments", 2) }, 0, 0, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b, west, central := pair()
			dr.Init(ctx, west, op)
			write(t, a, 6)
			a.ReplicateTo(b)
			tc.damage(b)

			// eu-west-1 is gone: its fence cannot be reached.
			res, err := dr.Promote(ctx, gone(west, a), central, op, 10*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if res.OldFenced {
				t.Fatal("claims to have fenced an unreachable region")
			}
			var rp *dr.ChainRepair
			for i := range res.Repairs {
				if res.Repairs[i].Repo == "payments" {
					rp = &res.Repairs[i]
				}
			}
			if tc.name == "consistent" {
				if rp != nil {
					t.Fatalf("repaired a consistent chain: %+v", rp)
				}
			} else if rp == nil || len(rp.Lost) != tc.lost || len(rp.Orphans) != tc.orphans {
				t.Fatalf("repair %+v", rp)
			}
			// The chain goes on and verifies, with the repair on record.
			write(t, b, 1)
			recs := verifyChain(t, b, "payments")
			want := tc.recordsB + 1
			if rp != nil {
				want++
			}
			if len(recs) != want {
				t.Fatalf("%d records, want %d", len(recs), want)
			}
			if rp != nil && recs[len(recs)-2].Action != "ChainRepairedAfterPromotion" {
				t.Fatalf("no repair record: %s", recs[len(recs)-2].Action)
			}
			if d := verifyChain(t, b, store.DRChain); !strings.Contains(string(d[len(d)-1].Detail), `"mode":"promote"`) {
				t.Fatal("promotion not on the DR chain")
			}
		})
	}
}

// An orphan (a record whose predecessor never arrived) is moved aside, so
// it does not block the seq new writes need, and is kept as evidence.
func TestPromoteQuarantinesOrphans(t *testing.T) {
	ctx := context.Background()
	a, b, west, central := pair()
	dr.Init(ctx, west, op)
	write(t, a, 6)
	a.ReplicateTo(b)
	// Record 5 never arrived, 6 did, and the head says 6.
	recs, _ := b.ListAudit(ctx, "payments", 0, 100)
	b.SimulateLostRecords("payments", 2) // drop 5 and 6, keep head 6
	b.ReplayRecord(recs[5])              // 6 arrives late
	res, err := dr.Promote(ctx, gone(west, a), central, op, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Repairs) == 0 || len(res.Repairs[0].Orphans) != 1 || len(res.Repairs[0].Lost) != 1 {
		t.Fatalf("repairs %+v", res.Repairs)
	}
	if len(b.Orphans) != 1 || b.Orphans[0].Seq != 6 {
		t.Fatalf("orphan not kept: %+v", b.Orphans)
	}
	write(t, b, 1)
	verifyChain(t, b, "payments")
}

// replicating is op with replication catching up each time switchover
// waits, as a global table does in the background.
func replicating(from, to *store.Memory) dr.Op {
	o := op
	o.Log = func(format string, _ ...any) {
		if strings.HasPrefix(format, "waiting") {
			from.ReplicateTo(to)
		}
	}
	return o
}

// gone is a region that cannot be reached.
func gone(r dr.Region, m *store.Memory) dr.Region {
	r.Meta = unreachable{m}
	r.Keys = func(context.Context) (map[string]bool, error) { return nil, errors.New("unreachable") }
	return r
}

// unreachable is a store whose writer change times out.
type unreachable struct{ *store.Memory }

func (unreachable) SetWriter(ctx context.Context, _ store.Writer, _ int64) error {
	<-ctx.Done()
	return ctx.Err()
}

// While a switchover drains, no region writes, even once the writer item has
// reached the target ahead of everything else; the target writes only after
// it holds everything.
func TestSwitchoverDrainsBeforeTheTargetWrites(t *testing.T) {
	a, b, west, central := pair()
	ctx := context.Background()
	dr.Init(ctx, west, op)
	write(t, a, 3)
	a.ReplicateTo(b)
	write(t, a, 2) // not yet replicated

	o := op
	var during []error
	waits := 0
	o.Log = func(format string, _ ...any) {
		if !strings.HasPrefix(format, "waiting") {
			return
		}
		waits++
		if waits == 1 {
			a.ReplicateWriterTo(b) // the writer item arrives first
			during = append(during, b.AppendAudit(ctx, "payments", entry(t, "ChangeAnnotated")), a.AppendAudit(ctx, "payments", entry(t, "ChangeAnnotated")))
			if err := b.Writable(ctx); err == nil || !strings.Contains(err.Error(), "paused") {
				t.Errorf("standby upload URLs while draining: %v", err)
			}
			return
		}
		a.ReplicateTo(b)
	}
	res, err := dr.Switchover(ctx, west, central, o, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range during {
		if !errors.Is(e, store.ErrNotWriter) {
			t.Fatalf("a region wrote while draining: %v", e)
		}
	}
	if res.Epoch != 3 {
		t.Fatalf("epoch %d, want 3 (drained at 2)", res.Epoch)
	}
	write(t, b, 1)
	if n := len(verifyChain(t, b, "payments")); n != 6 {
		t.Fatalf("%d records, want 6: nothing lost, nothing forked", n)
	}
}

// A switchover stopped while draining resumes when run again.
func TestSwitchoverResumesAfterTimeout(t *testing.T) {
	a, b, west, central := pair()
	ctx := context.Background()
	dr.Init(ctx, west, op)
	write(t, a, 2)
	a.ReplicateTo(b)
	if _, err := dr.Switchover(ctx, west, central, op, 20*time.Millisecond); err == nil {
		t.Fatal("switchover without replication succeeded")
	}
	if _, err := dr.Switchover(ctx, west, central, replicating(a, b), 5*time.Second); err != nil {
		t.Fatalf("resume: %v", err)
	}
	write(t, b, 1)
	verifyChain(t, b, "payments")
}
