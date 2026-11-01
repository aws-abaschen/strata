import * as iam from 'aws-cdk-lib/aws-iam'

/**
 * Key policy statements that let Cognito use a customer managed key for a
 * user pool in this account (any Region: a replica's key gets the same).
 * Pool ARNs use a wildcard: the pool's ARN depends on the key.
 */
export function cognitoKeyStatements(account: string, region: string): iam.PolicyStatement[] {
  const pools = `arn:aws:cognito-idp:${region}:${account}:userpool/*`
  const service = [new iam.ServicePrincipal('cognito-idp.amazonaws.com'), new iam.ServicePrincipal('identitystore.amazonaws.com')]
  const crypt = ['kms:Encrypt', 'kms:Decrypt', 'kms:ReEncrypt*', 'kms:GenerateDataKeyWithoutPlaintext']
  return [
    new iam.PolicyStatement({
      sid: 'CognitoService',
      principals: service,
      actions: crypt,
      resources: ['*'],
      conditions: {
        ArnLike: { 'aws:SourceArn': pools },
        StringEquals: { 'aws:SourceAccount': account },
        StringLike: { 'kms:EncryptionContext:aws:cognito-idp:userpool-arn': pools },
      },
    }),
    new iam.PolicyStatement({
      sid: 'CognitoServiceDescribe',
      principals: service,
      actions: ['kms:DescribeKey'],
      resources: ['*'],
      conditions: { ArnLike: { 'aws:SourceArn': pools }, StringEquals: { 'aws:SourceAccount': account } },
    }),
    new iam.PolicyStatement({
      sid: 'ViaCognito',
      principals: [new iam.AccountPrincipal(account)],
      actions: [...crypt, 'kms:DescribeKey'],
      resources: ['*'],
      conditions: { StringLike: { 'kms:ViaService': 'cognito-idp.*.amazonaws.com' } },
    }),
  ]
}
