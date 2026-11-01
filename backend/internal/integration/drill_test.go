//go:build aws

package integration

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	lambdasvc "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/aws-abaschen/strata/backend/internal/api"
	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/dr"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// TestDrill is a disaster-recovery drill against the deployed primary and
// standby: writes in the primary, a planned switchover to the standby,
// writes and an anchor there, a switchover back, and an export verified
// against the anchor key across both regions' writes. Run it with
//
//	STRATA_DRILL=1 AWS_PROFILE=admin go test -tags aws -count=1 -v -run TestDrill ./internal/integration/
//
// It leaves the primary as the writer, and its records on the strata-dr chain.
func TestDrill(t *testing.T) {
	if os.Getenv("STRATA_DRILL") == "" {
		t.Skip("STRATA_DRILL not set")
	}
	ctx := context.Background()
	const west, central = "eu-west-1", "eu-central-1"
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(west))
	if err != nil {
		t.Fatal(err)
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}
	type side struct {
		region dr.Region
		outs   map[string]string
		h      *harness
		cfg    aws.Config
	}
	sides := map[string]*side{}
	for _, s := range []struct{ region, stack string }{{west, "Strata"}, {central, "StrataStandby"}} {
		rc := cfg.Copy()
		rc.Region = s.region
		cf, err := cloudformation.NewFromConfig(rc).DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(s.stack)})
		if err != nil {
			t.Fatal(err)
		}
		outs := map[string]string{}
		for _, o := range cf.Stacks[0].Outputs {
			outs[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
		}
		s3c := s3.NewFromConfig(rc, func(o *s3.Options) { o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired })
		meta := &store.Dynamo{Client: dynamodb.NewFromConfig(rc), Table: outs["IndexTable"], Region: s.region, Primary: west}
		// The API handler runs here against that region's real stores, with
		// the fence on, as the region's Lambda does.
		a := &api.API{Objects: &store.S3{Client: s3c, Presign: s3.NewPresignClient(s3c), Bucket: outs["DataBucket"]}, Meta: meta,
			PolicyRepo: fmt.Sprintf("drill-nopolicy-%d", time.Now().UnixNano()), OpenBootstrap: true}
		handler := a.Handler()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := strings.TrimPrefix(r.Header.Get("authorization"), "Bearer ")
			handler.ServeHTTP(w, r.WithContext(api.WithPrincipal(r.Context(), api.ParseDevToken(tok))))
		}))
		t.Cleanup(srv.Close)
		bucket := outs["DataBucket"]
		sides[s.region] = &side{outs: outs, h: &harness{t: t, srv: srv}, cfg: rc, region: dr.Region{
			Name: s.region, API: outs["ApiUrl"], Meta: meta,
			Keys: func(ctx context.Context) (map[string]bool, error) {
				keys := map[string]bool{}
				p := s3.NewListObjectsV2Paginator(s3c, &s3.ListObjectsV2Input{Bucket: &bucket})
				for p.HasMorePages() {
					page, err := p.NextPage(ctx)
					if err != nil {
						return nil, err
					}
					for _, o := range page.Contents {
						keys[aws.ToString(o.Key)] = true
					}
				}
				return keys, nil
			},
		}}
	}
	W, C := sides[west], sides[central]
	op := dr.Op{Actor: aws.ToString(id.Arn), Reason: "integration DR drill (TestDrill)", WorkItem: "DRILL-" + time.Now().UTC().Format("20060102"),
		Log: func(f string, a ...any) { t.Logf(f, a...) }}

	// 0. Steady state: both regions agree before anything happens.
	waitInSync(t, W.region, C.region, 3*time.Minute)

	// 1. Writes in the primary.
	const ana = "drill-ana@example.com"
	repo := fmt.Sprintf("drill%d", time.Now().Unix())
	dir := t.TempDir()
	W.h.ok(dir, ana, "repo", "create", repo)
	W.h.ok(dir, ana, "init", "--repo", repo)
	write(t, dir, map[string]string{"README.md": "written in eu-west-1\n"})
	W.h.ok(dir, ana, "push", "-m", "DRILL-1 before switchover")

	// 2. The standby refuses writes and names the writer.
	e, _ := coreEntry(ana)
	var nw *store.NotWriterError
	if err := C.region.Meta.AppendAudit(ctx, repo, e); !errors.As(err, &nw) || nw.Writer.Region != west {
		t.Fatalf("standby wrote while not the writer: %v", err)
	}

	// 3. Planned switchover to the standby.
	res1, err := dr.Switchover(ctx, W.region, C.region, op, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("switchover %s -> %s: writes unavailable %s, %d chains, %d objects", west, central, res1.WritesUnavailable, res1.Chains, res1.Objects)

	// 4. The standby writes; the old primary refuses.
	write(t, dir, map[string]string{"README.md": "written in eu-central-1\n"})
	C.h.ok(dir, ana, "push", "-m", "DRILL-2 during the drill")
	if err := W.region.Meta.AppendAudit(ctx, repo, e); !errors.Is(err, store.ErrNotWriter) {
		t.Fatalf("demoted primary wrote: %v", err)
	}

	// 5. The standby's anchor job signs (with the primary's key, cross-region).
	rep := invokeAnchor(t, C.cfg, "StrataStandby-Anchor")
	if rep.Standby || rep.Anchored == 0 {
		t.Fatalf("standby anchor run while writer: %+v", rep)
	}
	t.Logf("anchor %d signed in %s", rep.Anchored, central)

	// 6. Fail back.
	res2, err := dr.Switchover(ctx, C.region, W.region, op, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("switchover %s -> %s: writes unavailable %s", central, west, res2.WritesUnavailable)
	write(t, dir, map[string]string{"README.md": "written in eu-west-1 again\n"})
	W.h.ok(dir, ana, "push", "-m", "DRILL-3 after failback")
	if rep := invokeAnchor(t, W.cfg, "Strata-Anchor"); rep.Standby {
		t.Fatalf("primary anchor run after failback: %+v", rep)
	}

	// 7. Evidence across both regions: export from the primary, verify with
	// the pinned anchor key. Records written in eu-central-1 are in it.
	pk, err := kms.NewFromConfig(W.cfg).GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(W.outs["AnchorKeyArn"])})
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "anchor.pem")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pk.PublicKey}), 0o644)
	exp := filepath.Join(t.TempDir(), "export")
	W.h.ok(dir, ana, "export", "--repo", repo, "--out", exp)
	out := W.h.ok(t.TempDir(), ana, "export", "verify", exp, "--anchor-key", keyFile)
	if !strings.Contains(out, "3 changes") || !strings.Contains(out, "signatures: verified with the pinned anchor key") {
		t.Fatalf("export after the drill:\n%s", out)
	}
	t.Logf("export verified:\n%s", out)
	waitInSync(t, W.region, C.region, 3*time.Minute)
}

func coreEntry(actor string) (core.AuditEntry, error) {
	return core.NewAuditEntry("ChangeAnnotated", actor, time.Now(), map[string]string{"note": "drill: must be refused"})
}

func waitInSync(t *testing.T, a, b dr.Region, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st, err := dr.Compare(context.Background(), a, b)
		if err != nil {
			t.Fatal(err)
		}
		if st.InSync {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("regions not in sync after %s: %v", timeout, st.Problems)
		}
		time.Sleep(3 * time.Second)
	}
}

type anchorReport struct {
	Anchored int64 `json:"anchored"`
	Standby  bool  `json:"standby"`
}

func invokeAnchor(t *testing.T, cfg aws.Config, prefix string) anchorReport {
	t.Helper()
	lc := lambdasvc.NewFromConfig(cfg)
	fns, err := lc.ListFunctions(context.Background(), &lambdasvc.ListFunctionsInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fns.Functions {
		if strings.HasPrefix(aws.ToString(f.FunctionName), prefix) {
			inv, err := lc.Invoke(context.Background(), &lambdasvc.InvokeInput{FunctionName: f.FunctionName})
			if err != nil || inv.FunctionError != nil {
				t.Fatalf("%s: %v %s", prefix, err, inv.Payload)
			}
			var rep anchorReport
			json.Unmarshal(inv.Payload, &rep)
			return rep
		}
	}
	t.Fatalf("no function %s* in %s", prefix, cfg.Region)
	return anchorReport{}
}
