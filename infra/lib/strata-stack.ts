import * as fs from 'node:fs'
import * as path from 'node:path'
import { Annotations, CfnOutput, Duration, RemovalPolicy, Stack, type StackProps } from 'aws-cdk-lib'
import * as apigw from 'aws-cdk-lib/aws-apigatewayv2'
import { HttpJwtAuthorizer } from 'aws-cdk-lib/aws-apigatewayv2-authorizers'
import { HttpLambdaIntegration } from 'aws-cdk-lib/aws-apigatewayv2-integrations'
import * as cloudfront from 'aws-cdk-lib/aws-cloudfront'
import * as origins from 'aws-cdk-lib/aws-cloudfront-origins'
import * as cognito from 'aws-cdk-lib/aws-cognito'
import * as dynamodb from 'aws-cdk-lib/aws-dynamodb'
import * as events from 'aws-cdk-lib/aws-events'
import * as iam from 'aws-cdk-lib/aws-iam'
import * as kms from 'aws-cdk-lib/aws-kms'
import * as targets from 'aws-cdk-lib/aws-events-targets'
import * as lambda from 'aws-cdk-lib/aws-lambda'
import * as logs from 'aws-cdk-lib/aws-logs'
import * as s3 from 'aws-cdk-lib/aws-s3'
import * as s3deploy from 'aws-cdk-lib/aws-s3-deployment'
import * as secrets from 'aws-cdk-lib/aws-secretsmanager'
import * as sqs from 'aws-cdk-lib/aws-sqs'
import * as cloudwatch from 'aws-cdk-lib/aws-cloudwatch'
import * as cwActions from 'aws-cdk-lib/aws-cloudwatch-actions'
import * as sns from 'aws-cdk-lib/aws-sns'
import * as subs from 'aws-cdk-lib/aws-sns-subscriptions'
import { DynamoEventSource, SqsDlq } from 'aws-cdk-lib/aws-lambda-event-sources'
import * as cr from 'aws-cdk-lib/custom-resources'
import type { Construct } from 'constructs'
import { allowLockedReplication, type PrimaryRefs } from './standby-base-stack.js'

const root = path.join(import.meta.dirname, '../..')

export interface StrataStackProps extends StackProps {
  /** Cognito hosted UI prefix, globally unique. Defaults to strata-<account>. */
  authDomainPrefix?: string
  /** Secrets Manager ARN holding {"baseUrl","email","apiToken"} for Jira. Optional. */
  jiraSecretArn?: string
  /** Days each audit record is locked in the archive bucket. Default 365. */
  auditRetentionDays?: number
  /**
   * Object Lock mode for the audit archive. 'governance' (default) can be
   * lifted by a privileged admin; 'compliance' cannot be lifted by anyone,
   * including the root user, until retention ends: use it in production only.
   */
  auditLockMode?: 'governance' | 'compliance'
  /** Repository holding the Cedar policies. Default strata-policies. */
  policyRepo?: string
  /** Email that receives ledger and tracker alarms (must confirm the subscription). */
  alarmEmail?: string
  /**
   * RFC 3161 timestamp authority for ledger anchors. Default FreeTSA, which is
   * not a qualified (eIDAS) authority: set a qualified one in production.
   */
  tsaUrl?: string
  /**
   * On the primary: the standby region's stores (outputs of StrataStandbyBase).
   * Adds the index table replica and data replication to it.
   */
  replica?: ReplicaRefs
  /**
   * Deploy as the warm standby: API, ledger, anchor and alarms over the
   * replicated table and the StrataStandbyBase stores. Writes are refused
   * (HTTP 421) until strata-dr makes this region the writer.
   */
  standby?: { primary: PrimaryRefs; base: StandbyStores }
}

/** The standby's stores as the primary sees them. */
export interface ReplicaRefs {
  region: string
  /** Replicate the data bucket too (default true; false keeps a replica-adding deploy to the table alone). */
  data?: boolean
  indexKeyArn: string
  dataBucket: string
  dataKeyArn: string
}

/** StrataStandbyBase outputs. */
export interface StandbyStores {
  indexKeyArn: string
  dataKeyArn: string
  auditKeyArn: string
  dataBucket: string
  auditBucket: string
}

/**
 * Strata, all serverless: S3 holds content, DynamoDB the index, Lambda the
 * API, EventBridge the integrations, CloudFront the web console.
 */
export class StrataStack extends Stack {
  constructor(scope: Construct, id: string, props: StrataStackProps = {}) {
    super(scope, id, props)

    // --- Content: objects and sealed changes, written once by key. ---
    // Object Lock is enabled now (it cannot be added later) so a compliance
    // retention period can be switched on per customer without a migration.
    // Customer-managed keys (rotated yearly), one per store so key custody can
    // be separated later. Retained on stack deletion: data stays readable.
    const standby = props.standby
    const primaryRegion = standby?.primary.region ?? this.region
    const key = (id: string, alias: string, imported?: string): kms.IKey =>
      imported
        ? kms.Key.fromKeyArn(this, id, imported)
        : new kms.Key(this, id, { alias: `alias/strata-${alias}`, enableKeyRotation: true, removalPolicy: RemovalPolicy.RETAIN })
    const dataKey = key('DataKey', 'data', standby?.base.dataKeyArn)
    const indexKey = key('IndexKey', 'index', standby?.base.indexKeyArn)
    const auditKey = key('AuditKey', 'audit', standby?.base.auditKeyArn)

    const data: s3.IBucket = standby
      ? s3.Bucket.fromBucketAttributes(this, 'Data', { bucketName: standby.base.dataBucket, encryptionKey: dataKey })
      : this.primaryData(dataKey, props.replica)
    const table = standby ? this.replicaTable(standby.primary, indexKey) : this.primaryTable(indexKey, props.replica)

    this.build(props, { standby, primaryRegion, dataKey, auditKey, data, table })
  }

  private primaryData(dataKey: kms.IKey, replica?: ReplicaRefs): s3.Bucket {
    const bucket = new s3.Bucket(this, 'Data', {
      objectLockEnabled: true,
      versioned: true,
      encryption: s3.BucketEncryption.KMS,
      encryptionKey: dataKey,
      bucketKeyEnabled: true,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.RETAIN,
      lifecycleRules: [{ abortIncompleteMultipartUploadAfter: Duration.days(7) }],
      // To the standby, so it can serve every change after a promotion.
      replicationRules: replica && replica.data !== false
        ? [
            {
              id: 'ToStandby',
              destination: s3.Bucket.fromBucketAttributes(this, 'StandbyData', { bucketName: replica.dataBucket, region: replica.region }),
              kmsKey: kms.Key.fromKeyArn(this, 'StandbyDataKey', replica.dataKeyArn),
              sseKmsEncryptedObjects: true,
              deleteMarkerReplication: false,
              priority: 1,
            },
          ]
        : undefined,
    })
    allowLockedReplication(bucket)
    return bucket
  }

  // --- Index: repos, refs, changes, work-item and label lookups. A global
  // table once a standby exists: replicated asynchronously, item by item,
  // last writer wins, so only one region writes (see store.Writer). ---
  private primaryTable(indexKey: kms.IKey, replica?: ReplicaRefs): dynamodb.ITableV2 {
    return new dynamodb.TableV2(this, 'Index', {
      encryption: dynamodb.TableEncryptionV2.customerManagedKey(indexKey, replica ? { [replica.region]: replica.indexKeyArn } : undefined),
      replicas: replica ? [{ region: replica.region, deletionProtection: true }] : undefined,
      partitionKey: { name: 'PK', type: dynamodb.AttributeType.STRING },
      sortKey: { name: 'SK', type: dynamodb.AttributeType.STRING },
      billing: dynamodb.Billing.onDemand(),
      pointInTimeRecoverySpecification: { pointInTimeRecoveryEnabled: true },
      // The audit records stream to the ledger Lambda (archive + events).
      // Global tables need both images; the ledger also uses the old one to
      // report overwritten records.
      dynamoStream: dynamodb.StreamViewType.NEW_AND_OLD_IMAGES,
      deletionProtection: true,
      removalPolicy: RemovalPolicy.RETAIN,
      globalSecondaryIndexes: [
        {
          indexName: 'GSI1',
          partitionKey: { name: 'GSI1PK', type: dynamodb.AttributeType.STRING },
          sortKey: { name: 'GSI1SK', type: dynamodb.AttributeType.STRING },
        },
      ],
    })
  }

  /** The table's replica in this region, with this region's stream. */
  private replicaTable(primary: PrimaryRefs, indexKey: kms.IKey): dynamodb.ITableV2 {
    const tableArn = this.formatArn({ service: 'dynamodb', resource: 'table', resourceName: primary.tableName })
    const call: cr.AwsSdkCall = {
      service: 'DynamoDB',
      action: 'describeTable',
      parameters: { TableName: primary.tableName },
      physicalResourceId: cr.PhysicalResourceId.of(`${primary.tableName}-stream`),
      outputPaths: ['Table.LatestStreamArn'],
    }
    const stream = new cr.AwsCustomResource(this, 'IndexStream', {
      onCreate: call,
      onUpdate: call,
      policy: cr.AwsCustomResourcePolicy.fromSdkCalls({ resources: [tableArn] }),
      installLatestAwsSdk: false,
    })
    return dynamodb.TableV2.fromTableAttributes(this, 'Index', {
      tableName: primary.tableName,
      tableStreamArn: stream.getResponseField('Table.LatestStreamArn'),
      encryptionKey: indexKey,
    })
  }

  private build(
    props: StrataStackProps,
    s: {
      standby?: { primary: PrimaryRefs; base: StandbyStores }
      primaryRegion: string
      dataKey: kms.IKey
      auditKey: kms.IKey
      data: s3.IBucket
      table: dynamodb.ITableV2
    },
  ) {
    const { standby, primaryRegion, auditKey, data, table } = s
    const regionEnv = { STRATA_REGION: this.region, PRIMARY_REGION: primaryRegion }

    // --- Events: every repo, ref and change event, archived for replay. ---
    const bus = new events.EventBus(this, 'Bus', { eventBusName: `strata-${this.stackName.toLowerCase()}` })
    bus.archive('Archive', {
      eventPattern: { source: ['strata'] },
      retention: Duration.days(365),
    })

    // --- Web console and identity: in the primary only. The standby accepts
    // the primary's Cognito tokens (user pools do not replicate; production
    // federates an IdP that outlives a region). ---
    const web = standby ? undefined : this.webAndIdentity(props, data as s3.Bucket)
    const webUrl = web?.webUrl ?? standby!.primary.webUrl
    const webOrigins = [webUrl, 'http://localhost:5173']
    const authority = web?.users.userPoolProviderUrl ?? standby!.primary.authority
    const clientId = web?.client.userPoolClientId ?? standby!.primary.clientId

    // --- Lambdas (Go, arm64, built into backend/dist by `just build`). ---
    const goLambda = (id: string, bin: string, extra: Partial<lambda.FunctionProps> = {}) =>
      new lambda.Function(this, id, {
        runtime: lambda.Runtime.PROVIDED_AL2023,
        architecture: lambda.Architecture.ARM_64,
        handler: 'bootstrap',
        code: lambda.Code.fromAsset(path.join(root, 'backend/dist', bin)),
        memorySize: 512,
        timeout: Duration.seconds(29),
        logGroup: new logs.LogGroup(this, `${id}Logs`, {
          retention: logs.RetentionDays.THREE_MONTHS,
          removalPolicy: RemovalPolicy.DESTROY,
        }),
        ...extra,
      })

    const apiFn = goLambda('Api', 'api', {
      environment: {
        BUCKET_NAME: data.bucketName,
        TABLE_NAME: table.tableName,
        WEB_URL: webUrl,
        ...regionEnv,
        POLICY_REPO: props.policyRepo ?? 'strata-policies',
      },
    })
    // PutObject is needed to sign upload URLs; clients then upload directly.
    data.grantReadWrite(apiFn)
    table.grantReadWriteData(apiFn)

    const api = new apigw.HttpApi(this, 'Http', {
      corsPreflight: {
        allowOrigins: webOrigins,
        allowHeaders: ['authorization', 'content-type'],
        allowMethods: [apigw.CorsHttpMethod.GET, apigw.CorsHttpMethod.POST, apigw.CorsHttpMethod.PUT],
        maxAge: Duration.hours(1),
      },
    })
    const stage = api.defaultStage?.node.defaultChild as apigw.CfnStage
    stage.defaultRouteSettings = { throttlingBurstLimit: 200, throttlingRateLimit: 100 }
    api.addRoutes({
      path: '/{proxy+}',
      // Explicit methods so CORS preflight never reaches the authorizer.
      methods: [apigw.HttpMethod.GET, apigw.HttpMethod.POST, apigw.HttpMethod.PUT],
      integration: new HttpLambdaIntegration('Lambda', apiFn),
      authorizer: new HttpJwtAuthorizer('Jwt', authority, { jwtAudience: [clientId] }),
    })

    // --- Work-tracker sync: links changes to Jira issues. ---
    const jiraSecret = props.jiraSecretArn
      ? secrets.Secret.fromSecretCompleteArn(this, 'JiraSecret', props.jiraSecretArn)
      : undefined
    // --- Audit ledger: every write's hash-chained record, archived under
    // Object Lock and published as an event (the ledger is the outbox). ---
    const retention = Duration.days(props.auditRetentionDays ?? 365)
    const auditBucket: s3.IBucket = standby
      ? s3.Bucket.fromBucketAttributes(this, 'Audit', { bucketName: standby.base.auditBucket, encryptionKey: auditKey })
      : new s3.Bucket(this, 'Audit', {
      objectLockEnabled: true,
      objectLockDefaultRetention:
        props.auditLockMode === 'compliance'
          ? s3.ObjectLockRetention.compliance(retention)
          : s3.ObjectLockRetention.governance(retention),
      versioned: true,
      encryption: s3.BucketEncryption.KMS,
      encryptionKey: auditKey,
      bucketKeyEnabled: true,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.RETAIN,
    })
    const ledgerFn = goLambda('Ledger', 'ledger', {
      environment: { AUDIT_BUCKET: auditBucket.bucketName, EVENT_BUS: bus.eventBusName, ...regionEnv },
    })
    auditBucket.grantPut(ledgerFn)
    bus.grantPutEventsTo(ledgerFn)
    const ledgerDlq = new sqs.Queue(this, 'LedgerDeadLetters', { retentionPeriod: Duration.days(14), enforceSSL: true })
    ledgerFn.addEventSource(
      new DynamoEventSource(table, {
        startingPosition: lambda.StartingPosition.TRIM_HORIZON,
        batchSize: 100,
        bisectBatchOnError: true,
        retryAttempts: 20,
        onFailure: new SqsDlq(ledgerDlq),
        filters: [
          // Inserts are archived; a MODIFY or REMOVE of an audit item is a
          // fork between regions (or tampering) and raises an alarm.
          lambda.FilterCriteria.filter({
            dynamodb: { Keys: { PK: { S: lambda.FilterRule.beginsWith('AUDIT#') } } },
          }),
        ],
      }),
    )

    const trackerFn = goLambda('Tracker', 'tracker', {
      environment: jiraSecret ? { JIRA_SECRET_ARN: jiraSecret.secretArn } : {},
    })
    jiraSecret?.grantRead(trackerFn)
    const dlq = new sqs.Queue(this, 'TrackerDeadLetters', {
      retentionPeriod: Duration.days(14),
      enforceSSL: true,
    })
    new events.Rule(this, 'TrackerRule', {
      eventBus: bus,
      eventPattern: { source: ['strata'], detailType: ['ChangeCreated', 'ChangeAnnotated'] },
      targets: [new targets.LambdaFunction(trackerFn, { retryAttempts: 8, maxEventAge: Duration.hours(24), deadLetterQueue: dlq })],
    })


    // --- Operational alarms (DORA Art. 17 incident detection): the audit
    // ledger must never silently stop, and nobody may tamper with it. ---
    const alarmTopic = new sns.Topic(this, 'Alarms', { enforceSSL: true })
    if (props.alarmEmail) alarmTopic.addSubscription(new subs.EmailSubscription(props.alarmEmail))
    // Once the topic has a resource policy (EventBridge targets add one),
    // CloudWatch alarms must be allowed explicitly or every alarm fails to publish.
    alarmTopic.addToResourcePolicy(new iam.PolicyStatement({
      sid: 'CloudWatchAlarms',
      principals: [new iam.ServicePrincipal('cloudwatch.amazonaws.com')],
      actions: ['sns:Publish'],
      resources: [alarmTopic.topicArn],
      conditions: {
        StringEquals: { 'aws:SourceAccount': this.account },
        ArnLike: { 'aws:SourceArn': `arn:${this.partition}:cloudwatch:${this.region}:${this.account}:alarm:*` },
      },
    }))
    const alarm = (id: string, metric: cloudwatch.IMetric, threshold: number, description: string) => {
      const a = new cloudwatch.Alarm(this, id, {
        metric,
        threshold,
        evaluationPeriods: 1,
        comparisonOperator: cloudwatch.ComparisonOperator.GREATER_THAN_THRESHOLD,
        treatMissingData: cloudwatch.TreatMissingData.NOT_BREACHING,
        alarmDescription: description,
      })
      a.addAlarmAction(new cwActions.SnsAction(alarmTopic))
      return a
    }
    const fiveMin = Duration.minutes(5)
    alarm(
      'LedgerLagAlarm',
      new cloudwatch.Metric({
        namespace: 'AWS/Lambda',
        metricName: 'IteratorAge',
        dimensionsMap: { FunctionName: ledgerFn.functionName },
        statistic: 'Maximum',
        period: fiveMin,
      }),
      5 * 60 * 1000,
      'The audit ledger is more than 5 minutes behind the table stream. The stream keeps 24 hours.',
    )
    alarm('LedgerErrorsAlarm', ledgerFn.metricErrors({ period: fiveMin }), 0, 'The audit ledger Lambda is failing.')
    alarm('LedgerDlqAlarm', ledgerDlq.metricApproximateNumberOfMessagesVisible({ period: fiveMin }), 0, 'Audit records are in the ledger dead-letter queue and not archived.')
    alarm(
      'LedgerForkAlarm',
      new cloudwatch.Metric({ namespace: 'Strata', metricName: 'LedgerRecordsOverwritten', statistic: 'Sum', period: fiveMin }),
      0,
      'An audit record changed or disappeared in this region: two regions wrote the same seq (split brain, replication kept one) or someone tampered with the table. See the ledger Lambda log for the key and both hashes.',
    )
    alarm('TrackerDlqAlarm', dlq.metricApproximateNumberOfMessagesVisible({ period: fiveMin }), 0, 'Work-tracker events failed after all retries.')

    // Someone disabling or changing the ledger is an incident. CloudTrail
    // management events reach the default event bus without extra setup.
    const trail = { source: ['aws.lambda'], detailType: ['AWS API Call via CloudTrail'] }
    for (const [id, detail] of [
      ['LedgerMappingChanged', { eventName: [{ prefix: 'UpdateEventSourceMapping' }, { prefix: 'DeleteEventSourceMapping' }] }],
      ['LedgerCodeChanged', { eventName: [{ prefix: 'UpdateFunctionCode' }, { prefix: 'UpdateFunctionConfiguration' }, { prefix: 'DeleteFunction' }], requestParameters: { functionName: [ledgerFn.functionName] } }],
    ] as const) {
      new events.Rule(this, id, {
        eventPattern: { ...trail, detail },
        targets: [new targets.SnsTopic(alarmTopic)],
        description: 'Audit ledger configuration changed',
      })
    }
    // --- Break-glass (DORA Art. 9 privileged access): every activation, early
    // end and review pages the alarm topic, and grants left unreviewed for
    // 24 hours after they end raise an alarm. ---
    new events.Rule(this, 'BreakGlassRule', {
      eventBus: bus,
      eventPattern: { source: ['strata'], detailType: ['BreakGlassActivated', 'BreakGlassEnded', 'BreakGlassReviewed'] },
      targets: [new targets.SnsTopic(alarmTopic)],
      description: 'Break-glass grant activated, ended or reviewed',
    })
    const sweeperFn = goLambda('Sweeper', 'sweeper', {
      environment: { TABLE_NAME: table.tableName, ...regionEnv },
      timeout: Duration.minutes(1),
    })
    table.grantReadData(sweeperFn)
    new events.Rule(this, 'SweeperSchedule', {
      schedule: events.Schedule.rate(Duration.hours(1)),
      targets: [new targets.LambdaFunction(sweeperFn)],
    })
    const hourly = Duration.hours(1)
    alarm(
      'BreakGlassReviewOverdueAlarm',
      new cloudwatch.Metric({ namespace: 'Strata', metricName: 'BreakGlassOverdueReviews', statistic: 'Maximum', period: hourly }),
      0,
      'A break-glass grant ended more than 24 hours ago and nobody has reviewed it: strata breakglass list --open',
    )
    alarm('SweeperErrorsAlarm', sweeperFn.metricErrors({ period: hourly }), 0, 'The break-glass sweeper Lambda is failing, so overdue reviews go unnoticed.')

    // --- Ledger anchoring (SOX §802, DORA Art. 12, SOC 2 CC7): every 15
    // minutes, sign (KMS, the private key never leaves it) and timestamp
    // (RFC 3161) a Merkle root of every chain head, check that earlier
    // anchored heads are unchanged, and reconcile the Object Lock archive
    // against the table. In this single-account prototype the operator can
    // still use the key; production moves it to a separate security account.
    // The standby signs with the same key, in the primary's region: auditors
    // pin one key. While that region is down no anchors are written and
    // AnchorStalledAlarm fires; anchoring resumes when it is back.
    const anchorKey: kms.IKey = standby
      ? kms.Key.fromKeyArn(this, 'AnchorKey', standby.primary.anchorKeyArn)
      : new kms.Key(this, 'AnchorKey', {
          alias: 'alias/strata-anchor',
          description: 'Signs audit ledger anchors (ECDSA P-256)',
          keySpec: kms.KeySpec.ECC_NIST_P256,
          keyUsage: kms.KeyUsage.SIGN_VERIFY,
          removalPolicy: RemovalPolicy.RETAIN,
        })
    const anchorFn = goLambda('Anchor', 'anchor', {
      environment: {
        TABLE_NAME: table.tableName,
        AUDIT_BUCKET: auditBucket.bucketName,
        ANCHOR_KEY_ARN: anchorKey.keyArn,
        TSA_URL: props.tsaUrl ?? 'https://freetsa.org/tsr',
        ...regionEnv,
      },
      timeout: Duration.minutes(5),
      reservedConcurrentExecutions: 1, // one writer: anchors must never fork
    })
    table.grantReadWriteData(anchorFn)
    auditBucket.grantRead(anchorFn)
    auditBucket.grantPut(anchorFn)
    anchorKey.grant(anchorFn, 'kms:Sign')
    new events.Rule(this, 'AnchorSchedule', {
      schedule: events.Schedule.rate(Duration.minutes(15)),
      targets: [new targets.LambdaFunction(anchorFn, { retryAttempts: 0 })],
    })
    const quarter = Duration.minutes(15)
    const strataMax = (metricName: string, period = quarter) =>
      new cloudwatch.Metric({ namespace: 'Strata', metricName, statistic: 'Maximum', period })
    alarm('AnchorRewriteAlarm', strataMax('AnchorRewrites'), 0, 'An anchored audit record changed or disappeared from the table: the ledger was rewritten after the fact. Treat as a security incident.')
    alarm('LedgerArchiveGapAlarm', strataMax('LedgerArchiveGaps'), 0, 'Audit records are missing from the Object Lock archive. See the anchor Lambda log for the repository and seq.')
    alarm('LedgerArchiveMismatchAlarm', strataMax('LedgerArchiveMismatches'), 0, 'An Object Lock archive record differs from the table: one of them was altered.')
    alarm('AnchorTimestampAlarm', strataMax('AnchorTimestampFailures', hourly), 0, 'Anchors are being written without an RFC 3161 timestamp (timestamp authority unreachable).')
    alarm('AnchorErrorsAlarm', anchorFn.metricErrors({ period: quarter }), 0, 'The ledger anchor Lambda is failing.')
    new cloudwatch.Alarm(this, 'AnchorStalledAlarm', {
      metric: new cloudwatch.Metric({ namespace: 'Strata', metricName: 'AnchorRuns', statistic: 'Sum', period: hourly }),
      threshold: 1,
      evaluationPeriods: 1,
      comparisonOperator: cloudwatch.ComparisonOperator.LESS_THAN_THRESHOLD,
      treatMissingData: cloudwatch.TreatMissingData.BREACHING,
      alarmDescription: 'No successful ledger anchor run in the last hour.',
    }).addAlarmAction(new cwActions.SnsAction(alarmTopic))
    new events.Rule(this, 'AnchorCodeChanged', {
      eventPattern: {
        ...trail,
        detail: { eventName: [{ prefix: 'UpdateFunctionCode' }, { prefix: 'UpdateFunctionConfiguration' }, { prefix: 'DeleteFunction' }], requestParameters: { functionName: [anchorFn.functionName] } },
      },
      targets: [new targets.SnsTopic(alarmTopic)],
      description: 'Ledger anchor configuration changed',
    })

    if (web) this.webFiles(web, api.apiEndpoint)

    const out: Record<string, string> = {
      ApiUrl: api.apiEndpoint,
      WebUrl: webUrl,
      Authority: authority,
      ClientId: clientId,
      DataBucket: data.bucketName,
      IndexTable: table.tableName,
      EventBus: bus.eventBusName,
      TrackerDlq: dlq.queueName,
      AuditBucket: auditBucket.bucketName,
      AlarmTopic: alarmTopic.topicArn,
      AnchorKeyArn: anchorKey.keyArn,
      Role: standby ? 'standby' : 'primary',
    }
    if (web) {
      out.UserPoolId = web.users.userPoolId
      out.AuthDomain = web.domain.baseUrl()
    }
    for (const [k, v] of Object.entries(out)) new CfnOutput(this, k, { value: v })
  }

  private webAndIdentity(props: StrataStackProps, data: s3.Bucket) {
    const site = new s3.Bucket(this, 'Site', {
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      encryption: s3.BucketEncryption.S3_MANAGED,
      removalPolicy: RemovalPolicy.DESTROY,
      autoDeleteObjects: true,
    })
    const cdn = new cloudfront.Distribution(this, 'Cdn', {
      defaultBehavior: {
        origin: origins.S3BucketOrigin.withOriginAccessControl(site),
        viewerProtocolPolicy: cloudfront.ViewerProtocolPolicy.REDIRECT_TO_HTTPS,
        responseHeadersPolicy: cloudfront.ResponseHeadersPolicy.SECURITY_HEADERS,
      },
      defaultRootObject: 'index.html',
      // Client-side routes: serve the app for any unknown path.
      errorResponses: [403, 404].map((httpStatus) => ({
        httpStatus,
        responseHttpStatus: 200,
        responsePagePath: '/index.html',
        ttl: Duration.seconds(0),
      })),
      priceClass: cloudfront.PriceClass.PRICE_CLASS_100,
    })
    const webUrl = `https://${cdn.distributionDomainName}`
    const webOrigins = [webUrl, 'http://localhost:5173']

    data.addCorsRule({
      allowedMethods: [s3.HttpMethods.GET, s3.HttpMethods.HEAD],
      allowedOrigins: webOrigins,
      allowedHeaders: ['*'],
      maxAge: 3600,
    })

    // --- Identity: no self sign-up; admins invite users (or federate SAML/OIDC). ---
    const users = new cognito.UserPool(this, 'Users', {
      selfSignUpEnabled: false,
      signInAliases: { email: true },
      autoVerify: { email: true },
      standardAttributes: { email: { required: true, mutable: true } },
      passwordPolicy: { minLength: 12 },
      mfa: cognito.Mfa.REQUIRED,
      mfaSecondFactor: { otp: true, sms: false },
      accountRecovery: cognito.AccountRecovery.EMAIL_ONLY,
      removalPolicy: RemovalPolicy.RETAIN,
    })
    // Break-glass group: always allowed unless a policy explicitly forbids.
    new cognito.CfnUserPoolGroup(this, 'AdminsGroup', {
      userPoolId: users.userPoolId,
      groupName: 'strata-admins',
      description: 'Strata break-glass administrators',
    })
    const domain = users.addDomain('Domain', {
      cognitoDomain: { domainPrefix: props.authDomainPrefix || `strata-${this.account}` },
    })
    const client = users.addClient('Web', {
      generateSecret: false,
      oAuth: {
        flows: { authorizationCodeGrant: true },
        scopes: [cognito.OAuthScope.OPENID, cognito.OAuthScope.EMAIL, cognito.OAuthScope.PROFILE],
        callbackUrls: webOrigins.map((o) => `${o}/callback`),
        logoutUrls: webOrigins.map((o) => `${o}/`),
      },
      idTokenValidity: Duration.hours(1),
      accessTokenValidity: Duration.hours(1),
      refreshTokenValidity: Duration.days(7),
      preventUserExistenceErrors: true,
    })
    return { site, cdn, webUrl, users, domain, client }
  }

  // --- Web console files and its runtime config. ---
  private webFiles(web: ReturnType<StrataStack['webAndIdentity']>, apiUrl: string) {
    const webDist = path.join(root, 'web/dist')
    const config = {
      apiUrl,
      authority: web.users.userPoolProviderUrl,
      clientId: web.client.userPoolClientId,
      authDomain: web.domain.baseUrl(),
    }
    if (fs.existsSync(path.join(webDist, 'index.html'))) {
      new s3deploy.BucketDeployment(this, 'SiteFiles', {
        destinationBucket: web.site,
        sources: [s3deploy.Source.asset(webDist), s3deploy.Source.jsonData('config.json', config)],
        distribution: web.cdn,
        distributionPaths: ['/index.html', '/config.json'],
        memoryLimit: 512,
      })
    } else {
      Annotations.of(this).addWarningV2('strata:web-not-built', 'web/dist is missing: run `just build` to deploy the web console.')
    }
  }
}
