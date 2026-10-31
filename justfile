# Linux/macOS: just <task>. Windows (PowerShell): just win::<task>, see win.just.
mod win

set shell := ["bash", "-euo", "pipefail", "-c"]
set export

AWS_PROFILE := env_var_or_default("AWS_PROFILE", "admin")

# List recipes
default:
    @just --list

# Install dependencies
install:
    cd backend && go mod download
    pnpm install --frozen-lockfile

# Build the Lambdas (arm64), the CLI and the web console
build:
    cd backend && for c in api tracker ledger sweeper anchor; do GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -trimpath -ldflags="-s -w" -o dist/$c/bootstrap ./cmd/$c; done
    cd backend && go build -trimpath -o dist/strata ./cmd/strata
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
    cd backend && go install ./cmd/strata

# Synthesize the CDK app
synth: build
    cd infra && pnpm exec cdk synth -q

# Show infrastructure changes
diff: build
    cd infra && pnpm exec cdk diff Strata

# Deploy to AWS with $AWS_PROFILE
deploy: build
    cd infra && pnpm exec cdk deploy Strata --require-approval broadening

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
    rm -rf backend/dist infra/cdk.out web/dist
