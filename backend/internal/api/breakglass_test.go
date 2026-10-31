package api

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aws-abaschen/strata/backend/internal/store"
)

// In AWS, break-glass needs a sign-in in the last 15 minutes (the token's
// auth_time); a token without one is refused, which fails closed.
func TestBreakGlassNeedsFreshSignIn(t *testing.T) {
	mem := store.NewMemory()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	h := (&API{Objects: mem, Meta: mem, RequireFreshAuth: true, Now: func() time.Time { return now }}).LambdaHandler()
	activate := func(claims map[string]string) events.APIGatewayV2HTTPResponse {
		ev := events.APIGatewayV2HTTPRequest{
			RawPath:         "/break-glass",
			Body:            base64.StdEncoding.EncodeToString([]byte(`{"reason":"payments are down","workItem":"INC-1","repos":["app"]}`)),
			IsBase64Encoded: true,
			Headers:         map[string]string{"content-type": "application/json"},
		}
		ev.RequestContext.HTTP.Method = "POST"
		ev.RequestContext.Authorizer = &events.APIGatewayV2HTTPRequestContextAuthorizerDescription{
			JWT: &events.APIGatewayV2HTTPRequestContextAuthorizerJWTDescription{Claims: claims},
		}
		res, err := h(context.Background(), ev)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	admin := func(authTime string) map[string]string {
		c := map[string]string{"email": "root@example.com", "cognito:groups": "[strata-admins]"}
		if authTime != "" {
			c["auth_time"] = authTime
		}
		return c
	}
	unix := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

	for name, claims := range map[string]map[string]string{
		"no auth_time":  admin(""),
		"stale sign-in": admin(unix(now.Add(-16 * time.Minute))),
	} {
		if res := activate(claims); res.StatusCode != 403 || !strings.Contains(res.Body, "sign-in with MFA") {
			t.Fatalf("%s: %d %s", name, res.StatusCode, res.Body)
		}
	}
	if res := activate(admin(unix(now.Add(-5 * time.Minute)))); res.StatusCode != 201 {
		t.Fatalf("fresh sign-in: %d %s", res.StatusCode, res.Body)
	}
}
