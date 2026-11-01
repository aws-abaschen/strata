# Windows recipes in PowerShell 7: just win::<task> (or just win <task>)
set shell := ["pwsh", "-NoLogo", "-NoProfile", "-Command"]
set export

AWS_PROFILE := env_var_or_default("AWS_PROFILE", "admin")

# List recipes
default:
    @just --list win

# Install dependencies
install:
    cd backend && go mod download
    pnpm install --frozen-lockfile

# Build the Lambdas (arm64), the CLI and the web console
build:
    cd backend; $env:GOOS = 'linux'; $env:GOARCH = 'arm64'; $env:CGO_ENABLED = '0'; foreach ($c in 'api', 'tracker', 'ledger', 'sweeper', 'anchor') { go build -tags lambda.norpc -trimpath '-ldflags=-s -w' -o dist/$c/bootstrap ./cmd/$c; if ($LASTEXITCODE) { exit $LASTEXITCODE } }
    cd backend; go build -trimpath -o dist/strata.exe ./cmd/strata; go build -trimpath -o dist/strata-dr.exe ./cmd/strata-dr
    pnpm --filter @strata/web build

# Run all tests and checks
test: build
    cd backend && go vet ./... && go test ./...
    pnpm -r typecheck
    pnpm -r test

# Format Go code
fmt:
    cd backend && gofmt -w .

# Install the strata CLI into $GOPATH/bin
cli:
    cd backend && go install ./cmd/strata ./cmd/strata-dr

# Synthesize the CDK app
synth: build
    cd infra && pnpm exec cdk synth -q

# Show infrastructure changes
diff: build
    cd infra && pnpm exec cdk diff Strata

# Deploy to AWS with $AWS_PROFILE
deploy: build
    cd infra && pnpm exec cdk deploy Strata --require-approval broadening

# Disaster recovery (warm standby in eu-central-1): see docs/architecture.md
dr-status:
    cd backend; go run ./cmd/strata-dr status

# One-time after creating a standby: copy objects and the audit archive that predate replication
dr-backfill:
    function Out($s, $r, $k) { aws cloudformation describe-stacks --stack-name $s --region $r --query "Stacks[0].Outputs[?OutputKey=='$k'].OutputValue" --output text }; aws s3 sync "s3://$(Out Strata eu-west-1 DataBucket)" "s3://$(Out StrataStandbyBase eu-central-1 DataBucket)" --source-region eu-west-1 --region eu-central-1; aws s3 sync "s3://$(Out Strata eu-west-1 AuditBucket)" "s3://$(Out StrataStandbyBase eu-central-1 AuditBucket)" --source-region eu-west-1 --region eu-central-1

# Live drill: switchover to the standby and back, with writes, an anchor and a verified export
dr-drill:
    cd backend; $env:STRATA_DRILL = '1'; go test -tags aws -count=1 -v -timeout 40m -run TestDrill ./internal/integration/

# Deploy the public showcase site (site/) to strata.baschen.is
site-deploy:
    cd infra && pnpm exec cdk deploy StrataSiteCert StrataSite --require-approval never

# Print the stack outputs
outputs:
    aws cloudformation describe-stacks --stack-name Strata --query 'Stacks[0].Outputs[].[OutputKey,OutputValue]' --output table

# Point the local web console at the deployed stack
web-config:
    node scripts/web-config.mjs

# Run the API locally: in-memory store, no AWS, bearer token = user (insecure)
dev-api:
    cd backend && go run ./cmd/devserver

# Run the web console locally (needs web/public/config.json)
dev:
    pnpm --filter @strata/web dev

# Remove build output
clean:
    foreach ($p in 'backend/dist', 'infra/cdk.out', 'web/dist') { if (Test-Path $p) { Remove-Item -Recurse -Force $p } }
