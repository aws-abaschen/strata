import { CfnOutput, Duration, RemovalPolicy, Stack, type StackProps } from 'aws-cdk-lib'
import * as iam from 'aws-cdk-lib/aws-iam'
import * as kms from 'aws-cdk-lib/aws-kms'
import * as s3 from 'aws-cdk-lib/aws-s3'
import type { Construct } from 'constructs'

/** What the standby needs to know about the primary (literal values: no cross-region references). */
export interface PrimaryRefs {
  region: string
  /** Name of the index table (the global table keeps it in every region). */
  tableName: string
  dataBucket: string
  dataKeyArn: string
  /** The anchor signing key: the standby signs with it, cross-region. */
  anchorKeyArn: string
  /** Cognito issuer and client of the primary (the standby accepts its tokens). */
  authority: string
  clientId: string
  apiUrl: string
  /** The web console (links in events point there). */
  webUrl: string
}

export interface StandbyBaseStackProps extends StackProps {
  primary: PrimaryRefs
  auditRetentionDays?: number
  auditLockMode?: 'governance' | 'compliance'
}

/**
 * The standby region's stores, deployed before the primary adds its table
 * replica (the replica needs this region's key) and before the standby's
 * compute. Data objects replicate both ways: from the primary while it is the
 * writer, back to it after a promotion. Each region builds its own audit
 * archive from its own table stream, so the audit bucket is not replicated.
 */
export class StandbyBaseStack extends Stack {
  readonly indexKey: kms.Key
  readonly dataKey: kms.Key
  readonly auditKey: kms.Key
  readonly data: s3.Bucket
  readonly audit: s3.Bucket

  constructor(scope: Construct, id: string, props: StandbyBaseStackProps) {
    super(scope, id, props)
    const key = (id: string, alias: string) =>
      new kms.Key(this, id, { alias: `alias/strata-${alias}`, enableKeyRotation: true, removalPolicy: RemovalPolicy.RETAIN })
    this.dataKey = key('DataKey', 'data')
    this.indexKey = key('IndexKey', 'index')
    this.auditKey = key('AuditKey', 'audit')

    const primaryData = s3.Bucket.fromBucketAttributes(this, 'PrimaryData', {
      bucketName: props.primary.dataBucket,
      region: props.primary.region,
    })
    this.data = new s3.Bucket(this, 'Data', {
      objectLockEnabled: true,
      versioned: true,
      encryption: s3.BucketEncryption.KMS,
      encryptionKey: this.dataKey,
      bucketKeyEnabled: true,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.RETAIN,
      lifecycleRules: [{ abortIncompleteMultipartUploadAfter: Duration.days(7) }],
      // Back to the primary: objects pushed here after a promotion must be
      // there before failback. Content-addressed keys never conflict, and S3
      // does not replicate replicas, so the two rules cannot loop.
      replicationRules: [
        {
          id: 'ToPrimary',
          destination: primaryData,
          kmsKey: kms.Key.fromKeyArn(this, 'PrimaryDataKey', props.primary.dataKeyArn),
          sseKmsEncryptedObjects: true,
          deleteMarkerReplication: false,
          priority: 1,
        },
      ],
    })

    allowLockedReplication(this.data)

    const retention = Duration.days(props.auditRetentionDays ?? 365)
    this.audit = new s3.Bucket(this, 'Audit', {
      objectLockEnabled: true,
      objectLockDefaultRetention:
        props.auditLockMode === 'compliance'
          ? s3.ObjectLockRetention.compliance(retention)
          : s3.ObjectLockRetention.governance(retention),
      versioned: true,
      encryption: s3.BucketEncryption.KMS,
      encryptionKey: this.auditKey,
      bucketKeyEnabled: true,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.RETAIN,
    })

    const out: Record<string, string> = {
      IndexKeyArn: this.indexKey.keyArn,
      DataKeyArn: this.dataKey.keyArn,
      AuditKeyArn: this.auditKey.keyArn,
      DataBucket: this.data.bucketName,
      AuditBucket: this.audit.bucketName,
    }
    for (const [k, v] of Object.entries(out)) new CfnOutput(this, k, { value: v })
  }
}

/**
 * Replicating from a bucket with Object Lock enabled also needs the source
 * objects' retention and legal hold, which CDK's generated role lacks.
 */
export function allowLockedReplication(bucket: s3.Bucket) {
  const role = bucket.node.tryFindChild('ReplicationRole') as iam.Role | undefined
  role?.addToPrincipalPolicy(
    new iam.PolicyStatement({ actions: ['s3:GetObjectRetention', 's3:GetObjectLegalHold'], resources: [bucket.arnForObjects('*')] }),
  )
}
