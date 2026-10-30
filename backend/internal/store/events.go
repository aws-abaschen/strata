package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

// EventSource is the EventBridge "source" of every Strata event.
const EventSource = "strata"

// EventBridge publishes events to a bus. Integrations (Jira, Slack, SIEM,
// data lake) subscribe with rules instead of polling the API.
type EventBridge struct {
	Client *eventbridge.Client
	Bus    string
}

// Publish implements Events.
func (e *EventBridge) Publish(ctx context.Context, detailType string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	out, err := e.Client.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []types.PutEventsRequestEntry{{
		EventBusName: aws.String(e.Bus), Source: aws.String(EventSource), DetailType: aws.String(detailType), Detail: aws.String(string(b)),
	}}})
	if err != nil {
		return err
	}
	if out.FailedEntryCount > 0 {
		return fmt.Errorf("eventbridge: %s", aws.ToString(out.Entries[0].ErrorMessage))
	}
	return nil
}
