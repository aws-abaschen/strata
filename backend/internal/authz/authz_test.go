package authz

import (
	"errors"
	"testing"

	"github.com/cedar-policy/cedar-go"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

func TestParsePoliciesNamesPoliciesByFile(t *testing.T) {
	ps, err := ParsePolicies(map[string][]byte{
		"a.cedar":      []byte(`permit (principal, action, resource); forbid (principal, action == Action::"admin", resource);`),
		"team/b.cedar": []byte(`permit (principal in Group::"x", action, resource);`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a.cedar#0", "a.cedar#1", "team/b.cedar#0"} {
		if ps.Get(cedar.PolicyID(id)) == nil {
			t.Errorf("missing %s", id)
		}
	}
	if _, err := ParsePolicies(map[string][]byte{"bad.cedar": []byte("permit (")}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("want invalid, got %v", err)
	}
}
