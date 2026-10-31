// Command sweeper is the hourly Lambda that checks break-glass grants. It
// emits, as CloudWatch metrics (embedded metric format, no API calls):
//
//	Strata/BreakGlassOverdueReviews  grants unreviewed 24h after they ended
//	Strata/BreakGlassActive          grants in force now
//
// An alarm on the first pages the alarm topic.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/aws-abaschen/strata/backend/internal/store"
)

func main() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	meta := &store.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: os.Getenv("TABLE_NAME")}
	lambda.Start(func(ctx context.Context) error {
		gs, err := meta.ListGrants(ctx, 0)
		if err != nil {
			return err
		}
		now := time.Now()
		overdue, active := []string{}, 0
		for _, g := range gs {
			if g.OverdueAt(now) {
				overdue = append(overdue, g.ID)
			}
			if g.ActiveAt(now) {
				active++
			}
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"_aws": map[string]any{
				"Timestamp": now.UnixMilli(),
				"CloudWatchMetrics": []any{map[string]any{
					"Namespace":  "Strata",
					"Dimensions": [][]string{{}},
					"Metrics": []any{
						map[string]string{"Name": "BreakGlassOverdueReviews", "Unit": "Count"},
						map[string]string{"Name": "BreakGlassActive", "Unit": "Count"},
					},
				}},
			},
			"BreakGlassOverdueReviews": len(overdue),
			"BreakGlassActive":         active,
			"overdue":                  overdue,
		})
	})
}
