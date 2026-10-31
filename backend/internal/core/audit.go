package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// AuditEntry is what a write records about itself. The store turns it into
// the next AuditRecord of the repository's chain, in the same transaction
// as the write, so there is never a write without its record.
type AuditEntry struct {
	Action string          // RepoCreated, ChangeCreated, RefUpdated...
	Actor  string          // verified identity
	At     time.Time       // server time
	Detail json.RawMessage // the event payload
	// BreakGlass is the grant that let this write through, if any. It is also
	// in Detail (so it is hashed); here it lets the store index the record
	// under the grant in the same transaction.
	BreakGlass string
}

// NewAuditEntry builds an entry from any JSON-serializable detail.
func NewAuditEntry(action, actor string, at time.Time, detail any) (AuditEntry, error) {
	b, err := json.Marshal(detail)
	if err != nil {
		return AuditEntry{}, err
	}
	return AuditEntry{Action: action, Actor: actor, At: at.UTC().Truncate(time.Millisecond), Detail: b}, nil
}

// AuditRecord is one link of a repository's tamper-evident ledger. Hash
// covers every field and the previous record's hash, so changing, removing
// or reordering any record breaks every hash after it.
type AuditRecord struct {
	Repo   string          `json:"repo"`
	Seq    int64           `json:"seq"`
	Prev   Hash            `json:"prev"`
	Hash   Hash            `json:"hash"`
	At     time.Time       `json:"at"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Detail json.RawMessage `json:"detail"`
}

// NextAuditRecord chains an entry after the record (seq, hash). seq 0 and an
// empty hash start a new chain.
func NextAuditRecord(repo string, seq int64, prev Hash, e AuditEntry) (AuditRecord, error) {
	if len(e.Detail) == 0 {
		e.Detail = json.RawMessage("{}")
	}
	r := AuditRecord{Repo: repo, Seq: seq + 1, Prev: prev, At: e.At.UTC().Truncate(time.Millisecond), Actor: e.Actor, Action: e.Action, Detail: e.Detail}
	return r, r.seal()
}

func (r *AuditRecord) digest() (Hash, error) {
	c := *r
	c.Hash = ""
	var buf bytes.Buffer
	if err := json.Compact(&buf, c.Detail); err != nil {
		return "", fmt.Errorf("audit detail: %w", err)
	}
	c.Detail = buf.Bytes()
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

func (r *AuditRecord) seal() error {
	var buf bytes.Buffer
	if err := json.Compact(&buf, r.Detail); err != nil {
		return fmt.Errorf("audit detail: %w", err)
	}
	r.Detail = buf.Bytes()
	h, err := r.digest()
	r.Hash = h
	return err
}

// VerifyAudit checks a contiguous run of records. prev is the hash of the
// record before the first one ("" when the run starts at seq 1). It returns
// the hash of the last record.
func VerifyAudit(records []AuditRecord, prev Hash) (Hash, error) {
	for i, r := range records {
		if i > 0 && r.Seq != records[i-1].Seq+1 {
			return "", fmt.Errorf("audit: gap between seq %d and %d", records[i-1].Seq, r.Seq)
		}
		if i == 0 && r.Seq == 1 && prev != "" {
			return "", fmt.Errorf("audit: seq 1 must start the chain")
		}
		if r.Prev != prev {
			return "", fmt.Errorf("audit: seq %d does not link to the previous record", r.Seq)
		}
		h, err := r.digest()
		if err != nil {
			return "", err
		}
		if h != r.Hash {
			return "", fmt.Errorf("audit: seq %d was altered (hash mismatch)", r.Seq)
		}
		prev = r.Hash
	}
	return prev, nil
}
