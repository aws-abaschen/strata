package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// A merge past its time budget is refused as a bad request with the reason,
// before the Lambda timeout turns it into a bare 5xx.
func TestMergeTimeBudget(t *testing.T) {
	mem := store.NewMemory()
	a := &API{Objects: mem, Meta: mem}
	empty, _, err := core.Tree{}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	h := mem.Put("app", empty)

	late := &treeMerger{a: a, ctx: context.Background(), repo: "app", deadline: time.Now().Add(-time.Second)}
	_, err = late.load(h)
	if !errors.Is(err, core.ErrInvalid) || !strings.Contains(err.Error(), "its limit") {
		t.Fatalf("past the budget: %v", err)
	}

	inTime := &treeMerger{a: a, ctx: context.Background(), repo: "app", deadline: time.Now().Add(time.Minute)}
	if _, err := inTime.load(h); err != nil {
		t.Fatalf("inside the budget: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gone := &treeMerger{a: a, ctx: ctx, repo: "app"}
	if _, err := gone.load(h); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
}
