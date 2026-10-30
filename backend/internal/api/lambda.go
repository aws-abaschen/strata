package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// LambdaHandler adapts the API to an API Gateway HTTP API (payload v2) event.
// The identity comes from the JWT the gateway already verified.
func (a *API) LambdaHandler() func(context.Context, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	h := a.Handler()
	return func(ctx context.Context, ev events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		body := ev.Body
		if ev.IsBase64Encoded {
			b, err := base64.StdEncoding.DecodeString(body)
			if err != nil {
				return events.APIGatewayV2HTTPResponse{StatusCode: http.StatusBadRequest}, nil
			}
			body = string(b)
		}
		target := ev.RawPath
		if ev.RawQueryString != "" {
			target += "?" + ev.RawQueryString
		}
		req := httptest.NewRequestWithContext(WithIdentity(ctx, claimIdentity(ev)), ev.RequestContext.HTTP.Method, target, strings.NewReader(body))
		for k, v := range ev.Headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		res := events.APIGatewayV2HTTPResponse{StatusCode: rec.Code, Headers: map[string]string{}, Body: rec.Body.String()}
		for k := range rec.Header() {
			res.Headers[strings.ToLower(k)] = rec.Header().Get(k)
		}
		return res, nil
	}
}

// claimIdentity picks a human-readable, stable identity from JWT claims.
func claimIdentity(ev events.APIGatewayV2HTTPRequest) string {
	if ev.RequestContext.Authorizer == nil || ev.RequestContext.Authorizer.JWT == nil {
		return ""
	}
	c := ev.RequestContext.Authorizer.JWT.Claims
	for _, k := range []string{"email", "username", "cognito:username", "sub"} {
		if v := c[k]; v != "" {
			return v
		}
	}
	return ""
}
