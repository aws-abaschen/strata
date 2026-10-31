package store

import (
	"context"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Break-glass items:
//
//	PK=BG  SK=GRANT#<id>      the grant (attributes ended, reviewed once set)
//	PK=BG  SK=ACTIVE#<user>   the user's latest grant: grantId, expiresAt (ms),
//	                          ended; guards "one active grant per user"
const bgPK = "BG"

func ms(n int64) *types.AttributeValueMemberN {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(n, 10)}
}

func grantItem(g core.Grant) (map[string]types.AttributeValue, error) {
	extra := map[string]types.AttributeValue{}
	if g.EndedAt != nil {
		extra["ended"] = s("1")
	}
	if g.Review != nil {
		extra["reviewed"] = s("1")
	}
	return item(bgPK, "GRANT#"+g.ID, g, extra)
}

func activeItem(g core.Grant) (map[string]types.AttributeValue, error) {
	extra := map[string]types.AttributeValue{"grantId": s(g.ID), "expiresAt": ms(g.ExpiresAt.UnixMilli())}
	if g.EndedAt != nil {
		extra["ended"] = s("1")
	}
	return item(bgPK, "ACTIVE#"+g.User, map[string]string{"id": g.ID}, extra)
}

// PutGrant implements Meta.
func (d *Dynamo) PutGrant(ctx context.Context, g core.Grant, audit core.AuditEntry) error {
	act, err := activeItem(g)
	if err != nil {
		return err
	}
	gi, err := grantItem(g)
	if err != nil {
		return err
	}
	return d.transact(ctx, core.BreakGlassChain, audit, []types.TransactWriteItem{
		{Put: &types.Put{Item: act,
			ConditionExpression:       aws.String("attribute_not_exists(PK) OR expiresAt <= :now OR attribute_exists(ended)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":now": ms(g.CreatedAt.UnixMilli())}}},
		{Put: &types.Put{Item: gi, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
	})
}

// ActiveGrant implements Meta.
func (d *Dynamo) ActiveGrant(ctx context.Context, user string) (core.Grant, error) {
	it, err := d.get(ctx, bgPK, "ACTIVE#"+user)
	if err != nil {
		return core.Grant{}, err
	}
	ref, err := decode[struct {
		ID string `json:"id"`
	}](it)
	if err != nil {
		return core.Grant{}, err
	}
	return d.GetGrant(ctx, ref.ID)
}

// GetGrant implements Meta.
func (d *Dynamo) GetGrant(ctx context.Context, id string) (core.Grant, error) {
	it, err := d.get(ctx, bgPK, "GRANT#"+id)
	if err != nil {
		return core.Grant{}, err
	}
	return decode[core.Grant](it)
}

// ListGrants implements Meta.
func (d *Dynamo) ListGrants(ctx context.Context, limit int) ([]core.Grant, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :p AND begins_with(SK, :s)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s(bgPK), ":s": s("GRANT#")},
		ScanIndexForward:          aws.Bool(false),
		ConsistentRead:            aws.Bool(true),
	}, limit)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.Grant](items)
}

// EndGrant implements Meta.
func (d *Dynamo) EndGrant(ctx context.Context, g core.Grant, audit core.AuditEntry) error {
	gi, err := grantItem(g)
	if err != nil {
		return err
	}
	act, err := activeItem(g)
	if err != nil {
		return err
	}
	return d.transact(ctx, core.BreakGlassChain, audit, []types.TransactWriteItem{
		{Put: &types.Put{Item: gi, ConditionExpression: aws.String("attribute_exists(PK) AND attribute_not_exists(ended)")}},
		{Put: &types.Put{Item: act, ConditionExpression: aws.String("grantId = :id"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":id": s(g.ID)}}},
	})
}

// ReviewGrant implements Meta.
func (d *Dynamo) ReviewGrant(ctx context.Context, g core.Grant, audit core.AuditEntry) error {
	gi, err := grantItem(g)
	if err != nil {
		return err
	}
	return d.transact(ctx, core.BreakGlassChain, audit, []types.TransactWriteItem{
		{Put: &types.Put{Item: gi, ConditionExpression: aws.String("attribute_exists(PK) AND attribute_not_exists(reviewed)")}},
	})
}

// ListGrantActivity implements Meta.
func (d *Dynamo) ListGrantActivity(ctx context.Context, id string, limit int) ([]core.AuditRecord, error) {
	items, err := d.query(ctx, &dynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :p"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("BGACT#" + id)},
		ConsistentRead:            aws.Bool(true),
	}, limit)
	if err != nil {
		return nil, err
	}
	return decodeAll[core.AuditRecord](items)
}
