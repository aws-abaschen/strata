package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aws-abaschen/strata/backend/internal/store"
)

func TestLambdaAdapter(t *testing.T) {
	mem := store.NewMemory()
	h := (&API{Objects: mem, Meta: mem, OpenBootstrap: true}).LambdaHandler()
	ev := events.APIGatewayV2HTTPRequest{
		RawPath:         "/repos",
		Body:            base64.StdEncoding.EncodeToString([]byte(`{"name":"payments"}`)),
		IsBase64Encoded: true,
		Headers:         map[string]string{"content-type": "application/json"},
	}
	ev.RequestContext.HTTP.Method = "POST"
	ev.RequestContext.Authorizer = &events.APIGatewayV2HTTPRequestContextAuthorizerDescription{
		JWT: &events.APIGatewayV2HTTPRequestContextAuthorizerJWTDescription{Claims: map[string]string{"sub": "123", "email": "ana@example.com"}},
	}
	res, err := h(context.Background(), ev)
	if err != nil || res.StatusCode != 201 {
		t.Fatalf("status %d err %v body %s", res.StatusCode, err, res.Body)
	}
	var out map[string]any
	json.Unmarshal([]byte(res.Body), &out)
	if out["createdBy"] != "ana@example.com" || res.Headers["content-type"] != "application/json" {
		t.Fatalf("body %s headers %v", res.Body, res.Headers)
	}

	ev.RequestContext.Authorizer = nil
	res, _ = h(context.Background(), ev)
	if res.StatusCode != 401 {
		t.Fatalf("no JWT: want 401, got %d", res.StatusCode)
	}
}

func TestClaimPrincipalGroups(t *testing.T) {
	ev := events.APIGatewayV2HTTPRequest{}
	ev.RequestContext.Authorizer = &events.APIGatewayV2HTTPRequestContextAuthorizerDescription{
		JWT: &events.APIGatewayV2HTTPRequestContextAuthorizerJWTDescription{Claims: map[string]string{
			"email": "ana@example.com", "cognito:groups": "[strata-admins payments-devs]",
		}},
	}
	p := claimPrincipal(ev)
	if p.ID != "ana@example.com" || len(p.Groups) != 2 || p.Groups[1] != "payments-devs" {
		t.Fatalf("%+v", p)
	}
}
