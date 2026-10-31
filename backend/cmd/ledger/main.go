// Command ledger is the Lambda that archives audit records to S3 (Object
// Lock) and publishes them to EventBridge, from the index table's stream.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/aws-abaschen/strata/backend/internal/ledger"
)

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	l := &ledger.Ledger{
		Archive:   &ledger.S3Archive{Client: s3.NewFromConfig(cfg), Bucket: os.Getenv("AUDIT_BUCKET")},
		Publisher: &ledger.EventBridgePublisher{Client: eventbridge.NewFromConfig(cfg), Bus: os.Getenv("EVENT_BUS")},
		Region:    os.Getenv("STRATA_REGION"),
		Primary:   os.Getenv("PRIMARY_REGION"),
		OnOverwrite: func(o ledger.Overwrite) {
			// Embedded metric format: alarm LedgerRecordOverwrittenAlarm.
			b, _ := json.Marshal(map[string]any{
				"_aws": map[string]any{"Timestamp": time.Now().UnixMilli(), "CloudWatchMetrics": []any{map[string]any{
					"Namespace": "Strata", "Dimensions": [][]string{{}}, "Metrics": []any{map[string]string{"Name": "LedgerRecordsOverwritten", "Unit": "Count"}}}}},
				"LedgerRecordsOverwritten": 1, "key": o.Key, "event": o.Event, "old": o.Old, "new": o.New,
				"message": "an audit record changed or disappeared: a fork between regions or tampering",
			})
			fmt.Println(string(b))
		},
	}
	lambda.Start(l.Handle)
}
