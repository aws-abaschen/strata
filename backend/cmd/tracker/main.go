// Command tracker is the Lambda that links Strata changes to work trackers.
// It does nothing until a tracker secret is configured.
package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/aws-abaschen/strata/backend/internal/tracker"
)

func main() {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	t := &tracker.Tracker{Linkers: map[string]tracker.Linker{}, Log: logger}

	if arn := os.Getenv("JIRA_SECRET_ARN"); arn != "" {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			log.Fatal(err)
		}
		out, err := secretsmanager.NewFromConfig(cfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
		if err != nil {
			log.Fatal(err)
		}
		var jc tracker.JiraConfig
		if err := json.Unmarshal([]byte(aws.ToString(out.SecretString)), &jc); err != nil {
			log.Fatalf("jira secret: %v", err)
		}
		if jc.BaseURL != "" {
			t.Linkers["jira"] = &tracker.Jira{Config: jc, Log: logger}
		} else {
			logger.Warn("jira secret has no baseUrl; Jira linking is off")
		}
	}
	lambda.Start(t.Handle)
}
