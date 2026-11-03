import { test } from 'node:test'
import assert from 'node:assert/strict'
import { App } from 'aws-cdk-lib'
import { Match, Template } from 'aws-cdk-lib/assertions'
import { StandbyBaseStack } from '../lib/standby-base-stack.js'
import { StrataStack } from '../lib/strata-stack.js'

const primary = {
  region: 'eu-west-1',
  tableName: 'Strata-Index',
  dataBucket: 'strata-data-primary',
  dataKeyArn: 'arn:aws:kms:eu-west-1:111111111111:key/data',
  anchorKeyArn: 'arn:aws:kms:eu-west-1:111111111111:key/anchor',
  authority: 'https://cognito-idp.eu-west-1.amazonaws.com/eu-west-1_x',
  clientId: 'client',
  apiUrl: 'https://primary.example',
  webUrl: 'https://web.example',
}
const base = {
  indexKeyArn: 'arn:aws:kms:eu-central-1:111111111111:key/index',
  dataKeyArn: 'arn:aws:kms:eu-central-1:111111111111:key/data',
  auditKeyArn: 'arn:aws:kms:eu-central-1:111111111111:key/audit',
  dataBucket: 'strata-data-standby',
  auditBucket: 'strata-audit-standby',
}
const env = (region: string) => ({ account: '111111111111', region })

test('the primary replicates its table and data to the standby', () => {
  const t = Template.fromStack(
    new StrataStack(new App(), 'P', {
      env: env('eu-west-1'),
      replica: { region: 'eu-central-1', indexKeyArn: base.indexKeyArn, dataBucket: base.dataBucket, dataKeyArn: base.dataKeyArn },
    }),
  )
  t.hasResourceProperties('AWS::DynamoDB::GlobalTable', {
    StreamSpecification: { StreamViewType: 'NEW_AND_OLD_IMAGES' },
    Replicas: Match.arrayWith([
      Match.objectLike({ Region: 'eu-central-1', SSESpecification: { KMSMasterKeyId: base.indexKeyArn }, DeletionProtectionEnabled: true }),
    ]),
  })
  t.hasResourceProperties('AWS::S3::Bucket', {
    ReplicationConfiguration: Match.objectLike({
      Rules: [Match.objectLike({ Id: 'ToStandby', Destination: Match.objectLike({ Bucket: { 'Fn::Join': ['', Match.arrayWith([`:s3:::${base.dataBucket}`])] } }) })],
    }),
  })
  // The fence knows its region; the ledger sees overwrites, not only inserts.
  t.hasResourceProperties('AWS::Lambda::Function', {
    Environment: { Variables: Match.objectLike({ STRATA_REGION: 'eu-west-1', PRIMARY_REGION: 'eu-west-1' }) },
  })
  t.hasResourceProperties('AWS::CloudWatch::Alarm', { MetricName: 'LedgerRecordsOverwritten' })
})

test('the primary replicates its user pool and fails sign-in over automatically', () => {
  const t = Template.fromStack(
    new StrataStack(new App(), 'P2', {
      env: env('eu-west-1'),
      replica: { region: 'eu-central-1', indexKeyArn: base.indexKeyArn, dataBucket: base.dataBucket, dataKeyArn: base.dataKeyArn, userPool: true },
    }),
  )
  t.hasResourceProperties('AWS::Cognito::UserPoolReplica', { RegionName: 'eu-central-1' })
  t.hasResourceProperties('AWS::Route53::HealthCheck', { HealthCheckConfig: Match.objectLike({ Type: 'HTTPS', FullyQualifiedDomainName: 'cognito-idp.eu-west-1.amazonaws.com' }) })
  t.hasResourceProperties('AWS::Cognito::UserPoolDomain', { Routing: { Failover: Match.objectLike({ SecondaryRegion: 'eu-central-1' }) } })
  t.hasResourceProperties('AWS::Cognito::UserPool', { IssuerConfiguration: { Type: 'UPDATED' } })
  t.hasResourceProperties('AWS::ApiGatewayV2::Authorizer', {
    JwtConfiguration: Match.objectLike({ Issuer: { 'Fn::Join': ['', Match.arrayWith(['https://issuer-cognito-idp.eu-west-1.amazonaws.com/'])] } }),
  })
})

test('the standby activates its user pool replica', () => {
  const t = Template.fromStack(
    new StrataStack(new App(), 'S2', { env: env('eu-central-1'), standby: { primary: { ...primary, userPoolId: 'eu-west-1_x' }, base, userPoolReplica: true } }),
  )
  t.hasResourceProperties('AWS::Cognito::UserPoolRegionalConfigurationAttachment', { UserPoolId: 'eu-west-1_x', Status: 'ACTIVE' })
})

test('the standby runs over the replica with the primary identity and anchor key', () => {
  const t = Template.fromStack(new StrataStack(new App(), 'S', { env: env('eu-central-1'), standby: { primary, base } }))
  t.resourceCountIs('AWS::DynamoDB::GlobalTable', 0)
  t.resourceCountIs('AWS::Cognito::UserPool', 0)
  t.resourceCountIs('AWS::CloudFront::Distribution', 0)
  t.resourceCountIs('AWS::KMS::Key', 0)
  t.hasResourceProperties('AWS::Lambda::Function', {
    Environment: { Variables: Match.objectLike({ STRATA_REGION: 'eu-central-1', PRIMARY_REGION: 'eu-west-1', ANCHOR_KEY_ARN: primary.anchorKeyArn }) },
  })
  t.hasResourceProperties('AWS::ApiGatewayV2::Authorizer', {
    JwtConfiguration: { Issuer: primary.authority, Audience: [primary.clientId] },
  })
  t.hasResourceProperties('AWS::Lambda::EventSourceMapping', {
    FilterCriteria: { Filters: [{ Pattern: Match.stringLikeRegexp('AUDIT#') }] },
  })
  t.hasOutput('Role', { Value: 'standby' })
  // Lambdas query the replica's index too (a live drill caught this missing).
  const policies = JSON.stringify(t.findResources('AWS::IAM::Policy'))
  assert.match(policies, /table\/Strata-Index\/index\/\*/)
  t.hasResourceProperties('AWS::CloudWatch::Alarm', { MetricName: 'LedgerRecordsOverwritten' })
})

test('the standby base keeps its own archive and replicates data back', () => {
  const t = Template.fromStack(new StandbyBaseStack(new App(), 'B', { env: env('eu-central-1'), primary }))
  t.resourceCountIs('AWS::KMS::Key', 3)
  t.hasResourceProperties('AWS::IAM::Policy', {
    PolicyDocument: { Statement: Match.arrayWith([Match.objectLike({ Action: ['s3:GetObjectRetention', 's3:GetObjectLegalHold'] })]) },
  })
  const buckets = t.findResources('AWS::S3::Bucket')
  const replicated = Object.values(buckets).filter((b) => b.Properties.ReplicationConfiguration)
  assert.equal(replicated.length, 1, 'only the data bucket replicates; each region archives its own ledger')
  t.hasResourceProperties('AWS::S3::Bucket', {
    ReplicationConfiguration: Match.objectLike({
      Rules: [Match.objectLike({ Id: 'ToPrimary', Destination: Match.objectLike({ Bucket: { 'Fn::Join': ['', Match.arrayWith([`:s3:::${primary.dataBucket}`])] } }) })],
    }),
  })
})

test('the shared API name fails over to the writer region', () => {
  const apiDomain = { name: 'api.example.com', hostedZoneId: 'Z1', zoneName: 'example.com' }
  const p = Template.fromStack(new StrataStack(new App(), 'P3', { env: env('eu-west-1'), apiDomain }))
  const s = Template.fromStack(new StrataStack(new App(), 'S3', { env: env('eu-central-1'), apiDomain, standby: { primary, base } }))
  for (const [t, role] of [[p, 'PRIMARY'], [s, 'SECONDARY']] as const) {
    t.hasResourceProperties('AWS::Route53::RecordSet', { Name: 'api.example.com', Type: 'A', Failover: role })
    t.hasResourceProperties('AWS::Route53::HealthCheck', { HealthCheckConfig: Match.objectLike({ ResourcePath: '/health', Type: 'HTTPS' }) })
    // /health is the only route without the authorizer.
    t.hasResourceProperties('AWS::ApiGatewayV2::Route', { RouteKey: 'GET /health', AuthorizationType: 'NONE' })
    t.hasResourceProperties('AWS::ApiGatewayV2::Route', { RouteKey: 'GET /{proxy+}', AuthorizationType: 'JWT' })
  }
})
