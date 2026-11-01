// Command anchor is the scheduled Lambda that anchors the audit ledger: it
// checks that previously anchored chain heads are unchanged, reconciles the
// Object Lock archive against the table, then signs (KMS) and timestamps
// (RFC 3161) a Merkle root of all chain heads. See package anchor.
//
// Metrics (namespace Strata, embedded metric format):
//
//	AnchorRuns                 1 per successful run (alarm when missing)
//	AnchorsWritten             1 when an anchor was written
//	AnchorRewrites             anchored heads no longer in the table as anchored
//	LedgerArchiveGaps          records missing from the Object Lock archive
//	LedgerArchiveMismatches    archive records that differ from the table
//	AnchorTimestampFailures    anchors written without a timestamp
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/aws-abaschen/strata/backend/internal/anchor"
	"github.com/aws-abaschen/strata/backend/internal/ledger"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// kmsSigner signs digests with an asymmetric KMS key; the private key never
// leaves KMS.
type kmsSigner struct {
	client *kms.Client
	keyID  string
}

func (k kmsSigner) KeyID() string { return k.keyID }

func (k kmsSigner) Sign(ctx context.Context, digest []byte) ([]byte, error) {
	out, err := k.client.Sign(ctx, &kms.SignInput{
		KeyId: aws.String(k.keyID), Message: digest,
		MessageType: kmstypes.MessageTypeDigest, SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, err
	}
	return out.Signature, nil
}

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	job := &anchor.Job{
		Meta:    &store.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: os.Getenv("TABLE_NAME"), Region: os.Getenv("STRATA_REGION"), Primary: os.Getenv("PRIMARY_REGION")},
		Archive: &ledger.S3Archive{Client: s3.NewFromConfig(cfg), Bucket: os.Getenv("AUDIT_BUCKET")},
		// The key may live in another region (a standby signs with the
		// primary's key): call KMS in the key's region.
		Signer: kmsSigner{client: kms.NewFromConfig(cfg, func(o *kms.Options) {
			if r := arnRegion(os.Getenv("ANCHOR_KEY_ARN")); r != "" {
				o.Region = r
			}
		}), keyID: os.Getenv("ANCHOR_KEY_ARN")},
		Region: os.Getenv("STRATA_REGION"),
	}
	if u := os.Getenv("TSA_URL"); u != "" {
		job.TSA = anchor.HTTPTSA{URL: u}
	}
	// The report is also the invocation's result (drills read it).
	lambda.Start(func(ctx context.Context) (anchor.Report, error) {
		rep, err := job.Run(ctx)
		if err != nil {
			return rep, err
		}
		written, tsFail := 0, 0
		if rep.Anchored > 0 {
			written = 1
		}
		if rep.TimestampFailed {
			tsFail = 1
		}
		names := []string{"AnchorRuns", "AnchorsWritten", "AnchorRewrites", "LedgerArchiveGaps", "LedgerArchiveMismatches", "AnchorTimestampFailures"}
		var metrics []any
		for _, n := range names {
			metrics = append(metrics, map[string]string{"Name": n, "Unit": "Count"})
		}
		return rep, json.NewEncoder(os.Stdout).Encode(map[string]any{
			"_aws": map[string]any{
				"Timestamp":         time.Now().UnixMilli(),
				"CloudWatchMetrics": []any{map[string]any{"Namespace": "Strata", "Dimensions": [][]string{{}}, "Metrics": metrics}},
			},
			"AnchorRuns": 1, "AnchorsWritten": written, "AnchorRewrites": len(rep.Rewrites),
			"LedgerArchiveGaps": len(rep.Gaps), "LedgerArchiveMismatches": len(rep.Mismatches), "AnchorTimestampFailures": tsFail,
			"report": rep,
		})
	})
}

// arnRegion returns the region field of an ARN ("" if it has none).
func arnRegion(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	return parts[3]
}
