// Command ledger is the Lambda that archives audit records to S3 (Object
// Lock) and publishes them to EventBridge, from the index table's stream.
package main

import (
	"context"
	"log"
	"os"

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
	}
	lambda.Start(l.Handle)
}
