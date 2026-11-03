// Command api is the Strata API Lambda.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/aws-abaschen/strata/backend/internal/api"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	// Only add checksums S3 requires; presigned URLs carry their own SHA-256.
	s3c := s3.NewFromConfig(cfg, func(o *s3.Options) { o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired })
	a := &api.API{
		Objects:          &store.S3{Client: s3c, Presign: s3.NewPresignClient(s3c), Bucket: os.Getenv("BUCKET_NAME")},
		Meta:             &store.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: os.Getenv("TABLE_NAME"), Region: os.Getenv("STRATA_REGION"), Primary: os.Getenv("PRIMARY_REGION")},
		WebURL:           os.Getenv("WEB_URL"),
		PolicyRepo:       os.Getenv("POLICY_REPO"),
		RequireFreshAuth: true,
		Region:           os.Getenv("STRATA_REGION"),
		Log:              slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
	lambda.Start(a.LambdaHandler())
}
