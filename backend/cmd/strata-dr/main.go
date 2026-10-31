// Command strata-dr moves the writer between Strata's primary and standby
// regions. It calls DynamoDB and S3 directly with your AWS credentials, not
// the Strata API, so it works when the API's region or its sign-in is down.
//
//	strata-dr status
//	strata-dr init
//	strata-dr switchover --to eu-central-1 --reason "quarterly drill" --work-item OPS-7
//	strata-dr promote --to eu-central-1 --force --reason "eu-west-1 outage" --work-item INC-9
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/aws-abaschen/strata/backend/internal/dr"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

const usage = `strata-dr: move Strata's writer between regions (AWS credentials, no Strata API)

  strata-dr status                     writer, chains and objects in both regions
  strata-dr init                       create the writer record (primary, epoch 1)
  strata-dr switchover --to REGION --reason TEXT --work-item KEY [--timeout 15m]
      planned: stop writes in the writer, wait until REGION holds everything, then
      REGION writes. No data loss. Use for drills and failback.
  strata-dr promote --to REGION --force --reason TEXT --work-item KEY
      the writer region is down: take the writer in REGION now and repair chains
      that hold part of a transaction. Writes not yet replicated are lost, and
      the ledger says which.

Flags for every command:
  --primary eu-west-1 --primary-stack Strata --standby eu-central-1 --standby-stack StrataStandby
`

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" {
		fmt.Print(usage)
		return nil
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	primary := fs.String("primary", "eu-west-1", "primary region")
	primaryStack := fs.String("primary-stack", "Strata", "primary stack")
	standby := fs.String("standby", "eu-central-1", "standby region")
	standbyStack := fs.String("standby-stack", "StrataStandby", "standby stack")
	to := fs.String("to", "", "region to make the writer")
	reason := fs.String("reason", "", "why (recorded on the DR chain)")
	workItem := fs.String("work-item", "", "change or incident ticket")
	timeout := fs.Duration("timeout", 15*time.Minute, "switchover: how long to wait for the target to catch up")
	force := fs.Bool("force", false, "promote: confirm you accept losing writes not yet replicated")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	regions := map[string]dr.Region{}
	for _, side := range []struct{ region, stack string }{{*primary, *primaryStack}, {*standby, *standbyStack}} {
		r, err := region(ctx, cfg, side.region, side.stack, *primary)
		if err != nil {
			return err
		}
		regions[side.region] = r
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return err
	}
	op := dr.Op{Actor: aws.ToString(id.Arn), Reason: *reason, WorkItem: *workItem,
		Log: func(f string, a ...any) { fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+f+"\n", a...) }}

	pick := func() (from, target dr.Region, err error) {
		t, ok := regions[*to]
		if !ok {
			return from, target, fmt.Errorf("--to must be %s or %s", *primary, *standby)
		}
		for n, r := range regions {
			if n != *to {
				from = r
			}
		}
		return from, t, nil
	}

	var out any
	switch cmd {
	case "status":
		st, err := dr.Compare(ctx, regions[*primary], regions[*standby])
		if err != nil {
			return err
		}
		if *asJSON {
			out = st
			break
		}
		printStatus(st, *primary, *standby)
		return nil
	case "init":
		w, err := dr.Init(ctx, regions[*primary], op)
		if errors.Is(err, store.ErrConflict) {
			return errors.New("a writer record already exists: see status")
		}
		if err != nil {
			return err
		}
		out = w
	case "switchover":
		from, target, err := pick()
		if err != nil {
			return err
		}
		res, err := dr.Switchover(ctx, from, target, op, *timeout)
		if err != nil {
			return err
		}
		out = res
	case "promote":
		if !*force {
			return errors.New("promote loses writes not yet replicated: pass --force (use switchover if both regions are up)")
		}
		from, target, err := pick()
		if err != nil {
			return err
		}
		res, err := dr.Promote(ctx, from, target, op, 10*time.Second)
		if err != nil {
			return err
		}
		out = res
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}

// region reads a stack's outputs and builds that side.
func region(ctx context.Context, cfg aws.Config, name, stack, primary string) (dr.Region, error) {
	rc := cfg.Copy()
	rc.Region = name
	cf, err := cloudformation.NewFromConfig(rc).DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: &stack})
	if err != nil {
		return dr.Region{}, fmt.Errorf("stack %s in %s: %w", stack, name, err)
	}
	outs := map[string]string{}
	for _, o := range cf.Stacks[0].Outputs {
		outs[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}
	for _, k := range []string{"IndexTable", "DataBucket", "ApiUrl"} {
		if outs[k] == "" {
			return dr.Region{}, fmt.Errorf("stack %s in %s has no %s output", stack, name, k)
		}
	}
	s3c := s3.NewFromConfig(rc)
	bucket := outs["DataBucket"]
	return dr.Region{
		Name: name,
		API:  outs["ApiUrl"],
		Meta: &store.Dynamo{Client: dynamodb.NewFromConfig(rc), Table: outs["IndexTable"], Region: name, Primary: primary},
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
	}, nil
}

func printStatus(st dr.Status, primary, standby string) {
	for _, r := range []string{primary, standby} {
		w := st.Writer[r]
		fmt.Printf("%-13s sees writer %s (epoch %d)", r, w.Region, w.Epoch)
		if !w.Since.IsZero() {
			fmt.Printf(" since %s by %s", w.Since.Format(time.RFC3339), w.By)
		}
		fmt.Printf("; %d objects\n", st.Objects[r])
	}
	same := 0
	for _, c := range st.Chains {
		if c.Same() {
			same++
		}
	}
	fmt.Printf("chains: %d of %d identical in both regions\n", same, len(st.Chains))
	if st.InSync {
		fmt.Println("in sync: a switchover would complete without waiting")
		return
	}
	slices.Sort(st.Problems)
	for _, p := range st.Problems {
		fmt.Println("  " + p)
	}
}
