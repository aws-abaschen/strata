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
import * as targets from 'aws-cdk-lib/aws-events-targets'
import * as lambda from 'aws-cdk-lib/aws-lambda'
import * as logs from 'aws-cdk-lib/aws-logs'
import * as s3 from 'aws-cdk-lib/aws-s3'
import * as s3deploy from 'aws-cdk-lib/aws-s3-deployment'
import * as secrets from 'aws-cdk-lib/aws-secretsmanager'
import * as sqs from 'aws-cdk-lib/aws-sqs'
import type { Construct } from 'constructs'

const root = path.join(import.meta.dirname, '../..')

export interface StrataStackProps extends StackProps {
  /** Cognito hosted UI prefix, globally unique. Defaults to strata-<account>. */
  authDomainPrefix?: string
  /** Secrets Manager ARN holding {"baseUrl","email","apiToken"} for Jira. Optional. */
  jiraSecretArn?: string
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
    const data = new s3.Bucket(this, 'Data', {
      objectLockEnabled: true,
      versioned: true,
      encryption: s3.BucketEncryption.S3_MANAGED,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.RETAIN,
      lifecycleRules: [{ abortIncompleteMultipartUploadAfter: Duration.days(7) }],
    })

    // --- Index: repos, refs, changes, work-item and label lookups. ---
    const table = new dynamodb.TableV2(this, 'Index', {
      partitionKey: { name: 'PK', type: dynamodb.AttributeType.STRING },
      sortKey: { name: 'SK', type: dynamodb.AttributeType.STRING },
      billing: dynamodb.Billing.onDemand(),
      pointInTimeRecoverySpecification: { pointInTimeRecoveryEnabled: true },
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

    // --- Events: every repo, ref and change event, archived for replay. ---
    const bus = new events.EventBus(this, 'Bus', { eventBusName: `strata-${this.stackName.toLowerCase()}` })
    bus.archive('Archive', {
      eventPattern: { source: ['strata'] },
      retention: Duration.days(365),
    })

    // --- Web console hosting (built from ../web/dist). ---
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
      mfa: cognito.Mfa.OPTIONAL,
      mfaSecondFactor: { otp: true, sms: false },
      accountRecovery: cognito.AccountRecovery.EMAIL_ONLY,
      removalPolicy: RemovalPolicy.RETAIN,
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
        EVENT_BUS: bus.eventBusName,
        WEB_URL: webUrl,
      },
    })
    // PutObject is needed to sign upload URLs; clients then upload directly.
    data.grantReadWrite(apiFn)
    table.grantReadWriteData(apiFn)
    bus.grantPutEventsTo(apiFn)

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
      authorizer: new HttpJwtAuthorizer('Jwt', users.userPoolProviderUrl, {
        jwtAudience: [client.userPoolClientId],
      }),
    })

    // --- Work-tracker sync: links changes to Jira issues. ---
    const jiraSecret = props.jiraSecretArn
      ? secrets.Secret.fromSecretCompleteArn(this, 'JiraSecret', props.jiraSecretArn)
      : undefined
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

    // --- Web console files and its runtime config. ---
    const webDist = path.join(root, 'web/dist')
    const config = {
      apiUrl: api.apiEndpoint,
      authority: users.userPoolProviderUrl,
      clientId: client.userPoolClientId,
      authDomain: domain.baseUrl(),
    }
    if (fs.existsSync(path.join(webDist, 'index.html'))) {
      new s3deploy.BucketDeployment(this, 'SiteFiles', {
        destinationBucket: site,
        sources: [s3deploy.Source.asset(webDist), s3deploy.Source.jsonData('config.json', config)],
        distribution: cdn,
        distributionPaths: ['/index.html', '/config.json'],
        memoryLimit: 512,
      })
    } else {
      Annotations.of(this).addWarningV2('strata:web-not-built', 'web/dist is missing: run `just build` to deploy the web console.')
    }

    const out: Record<string, string> = {
      ApiUrl: api.apiEndpoint,
      WebUrl: webUrl,
      Authority: users.userPoolProviderUrl,
      UserPoolId: users.userPoolId,
      ClientId: client.userPoolClientId,
      AuthDomain: domain.baseUrl(),
      DataBucket: data.bucketName,
      IndexTable: table.tableName,
      EventBus: bus.eventBusName,
      TrackerDlq: dlq.queueName,
    }
    for (const [k, v] of Object.entries(out)) new CfnOutput(this, k, { value: v })
  }
}
