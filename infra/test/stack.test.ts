import { test } from 'node:test'
import { App } from 'aws-cdk-lib'
import { Match, Template } from 'aws-cdk-lib/assertions'
import { StrataStack } from '../lib/strata-stack.js'

const template = Template.fromStack(
  new StrataStack(new App(), 'Test', {
    env: { account: '111111111111', region: 'eu-west-1' },
    jiraSecretArn: 'arn:aws:secretsmanager:eu-west-1:111111111111:secret:jira-AbCdEf',
  }),
)

test('content bucket is private, locked-capable and retained', () => {
  template.hasResource('AWS::S3::Bucket', {
    DeletionPolicy: 'Retain',
    Properties: Match.objectLike({
      ObjectLockEnabled: true,
      VersioningConfiguration: { Status: 'Enabled' },
      PublicAccessBlockConfiguration: Match.objectLike({ BlockPublicAcls: true, RestrictPublicBuckets: true }),
    }),
  })
})

test('index table has PITR and deletion protection', () => {
  template.hasResourceProperties('AWS::DynamoDB::GlobalTable', {
    Replicas: Match.arrayWith([
      Match.objectLike({
        DeletionProtectionEnabled: true,
        PointInTimeRecoverySpecification: { PointInTimeRecoveryEnabled: true },
      }),
    ]),
  })
})

test('every API route requires a JWT', () => {
  const routes = template.findResources('AWS::ApiGatewayV2::Route')
  for (const [id, r] of Object.entries(routes)) {
    if ((r as any).Properties.AuthorizationType !== 'JWT') throw new Error(`route ${id} is not JWT-protected`)
  }
})

test('users cannot sign themselves up', () => {
  template.hasResourceProperties('AWS::Cognito::UserPool', {
    AdminCreateUserConfig: { AllowAdminCreateUserOnly: true },
  })
})

test('tracker gets change events with retries and a DLQ', () => {
  template.hasResourceProperties('AWS::Events::Rule', {
    EventPattern: { source: ['strata'], 'detail-type': ['ChangeCreated', 'ChangeAnnotated'] },
    Targets: Match.arrayWith([Match.objectLike({ RetryPolicy: { MaximumRetryAttempts: 8, MaximumEventAgeInSeconds: 86400 } })]),
  })
})

test('lambdas are arm64 Go custom runtime', () => {
  template.resourcePropertiesCountIs('AWS::Lambda::Function', { Runtime: 'provided.al2023', Architectures: ['arm64'] }, 4)
})

test('audit records stream to the ledger, archived under Object Lock', () => {
  template.hasResourceProperties('AWS::S3::Bucket', {
    ObjectLockConfiguration: { ObjectLockEnabled: 'Enabled', Rule: { DefaultRetention: { Mode: 'GOVERNANCE', Days: 365 } } },
  })
  template.hasResourceProperties('AWS::Lambda::EventSourceMapping', {
    FilterCriteria: { Filters: [{ Pattern: Match.stringLikeRegexp('AUDIT#') }] },
    BisectBatchOnFunctionError: true,
  })
})

test('ledger failures and tampering raise alarms', () => {
  template.resourceCountIs('AWS::CloudWatch::Alarm', 6)
  template.hasResourceProperties('AWS::CloudWatch::Alarm', { MetricName: 'BreakGlassOverdueReviews', Namespace: 'Strata' })
  template.hasResourceProperties('AWS::Events::Rule', { EventPattern: { 'detail-type': ['BreakGlassActivated', 'BreakGlassEnded', 'BreakGlassReviewed'] } })
  template.hasResourceProperties('AWS::CloudWatch::Alarm', { MetricName: 'IteratorAge', Threshold: 300000 })
  template.hasResourceProperties('AWS::Events::Rule', {
    EventPattern: Match.objectLike({ source: ['aws.lambda'], detail: Match.objectLike({ eventName: Match.arrayWith([{ prefix: 'UpdateEventSourceMapping' }]) }) }),
  })
})
