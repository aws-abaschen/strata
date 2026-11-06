import * as path from 'node:path'
import { fileURLToPath } from 'node:url'
import { CfnOutput, Duration, RemovalPolicy, Stack, type StackProps } from 'aws-cdk-lib'
import * as acm from 'aws-cdk-lib/aws-certificatemanager'
import * as cloudfront from 'aws-cdk-lib/aws-cloudfront'
import * as origins from 'aws-cdk-lib/aws-cloudfront-origins'
import * as route53 from 'aws-cdk-lib/aws-route53'
import * as targets from 'aws-cdk-lib/aws-route53-targets'
import * as s3 from 'aws-cdk-lib/aws-s3'
import * as deploy from 'aws-cdk-lib/aws-s3-deployment'
import type { Construct } from 'constructs'

// The public showcase site (site/). Separate from the Strata stack: it holds no
// customer data, so it is destroyed with its stack.

export interface SiteZone {
  domainName: string
  hostedZoneId: string
  zoneName: string
}

/** CloudFront certificates must live in us-east-1. */
export class SiteCertStack extends Stack {
  readonly certificate: acm.ICertificate

  constructor(scope: Construct, id: string, props: StackProps & { zone: SiteZone }) {
    super(scope, id, props)
    // The zone is owned by baschenis-cloud-ci/aws-route53; this only adds the validation record.
    const zone = route53.HostedZone.fromHostedZoneAttributes(this, 'Zone', props.zone)
    this.certificate = new acm.Certificate(this, 'Cert', {
      domainName: props.zone.domainName,
      validation: acm.CertificateValidation.fromDns(zone),
    })
  }
}

export interface SiteStackProps extends StackProps {
  zone?: SiteZone
  certificate?: acm.ICertificate
  siteDir?: string
}

export class SiteStack extends Stack {
  constructor(scope: Construct, id: string, props: SiteStackProps) {
    super(scope, id, props)

    const bucket = new s3.Bucket(this, 'Site', {
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      encryption: s3.BucketEncryption.S3_MANAGED,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.DESTROY,
      autoDeleteObjects: true,
    })

    // No scripts, no third-party origins. Pages may frame the interactive
    // figures under /figures/, which come from this origin only.
    const csp = [
      "default-src 'none'",
      "style-src 'self'",
      "img-src 'self' data:",
      "frame-src 'self'",
      "frame-ancestors 'none'",
      "base-uri 'none'",
      "form-action 'none'",
    ].join('; ')

    // /figures/*: same-origin script files (nothing inline, no eval), and only
    // this site may frame them.
    const figuresCsp = [
      "default-src 'none'",
      "script-src 'self'",
      "style-src 'self'",
      "img-src 'self' data:",
      "frame-ancestors 'self'",
      "base-uri 'none'",
      "form-action 'none'",
    ].join('; ')

    const security = (policy: string, frameOption: cloudfront.HeadersFrameOption) => ({
      contentSecurityPolicy: { contentSecurityPolicy: policy, override: true },
      strictTransportSecurity: { accessControlMaxAge: Duration.days(365), includeSubdomains: true, override: true },
      contentTypeOptions: { override: true },
      frameOptions: { frameOption, override: true },
      referrerPolicy: { referrerPolicy: cloudfront.HeadersReferrerPolicy.STRICT_ORIGIN_WHEN_CROSS_ORIGIN, override: true },
    })

    const headers = new cloudfront.ResponseHeadersPolicy(this, 'Headers', {
      securityHeadersBehavior: security(csp, cloudfront.HeadersFrameOption.DENY),
    })
    const figureHeaders = new cloudfront.ResponseHeadersPolicy(this, 'FigureHeaders', {
      securityHeadersBehavior: security(figuresCsp, cloudfront.HeadersFrameOption.SAMEORIGIN),
    })

    const domainNames = props.zone && props.certificate ? [props.zone.domainName] : undefined
    const origin = origins.S3BucketOrigin.withOriginAccessControl(bucket)
    const behavior = (responseHeadersPolicy: cloudfront.IResponseHeadersPolicy): cloudfront.BehaviorOptions => ({
      origin,
      viewerProtocolPolicy: cloudfront.ViewerProtocolPolicy.REDIRECT_TO_HTTPS,
      cachePolicy: cloudfront.CachePolicy.CACHING_OPTIMIZED,
      responseHeadersPolicy,
      compress: true,
    })
    const distribution = new cloudfront.Distribution(this, 'Cdn', {
      defaultRootObject: 'index.html',
      domainNames,
      certificate: props.certificate,
      priceClass: cloudfront.PriceClass.PRICE_CLASS_100,
      httpVersion: cloudfront.HttpVersion.HTTP2_AND_3,
      defaultBehavior: behavior(headers),
      additionalBehaviors: { '/figures/*': behavior(figureHeaders) },
      // Without ListBucket, S3 answers 403 for a missing key.
      errorResponses: [403, 404].map((httpStatus) => ({
        httpStatus,
        responseHttpStatus: 404,
        responsePagePath: '/404.html',
        ttl: Duration.minutes(5),
      })),
    })

    // Plain files without content hashes: short cache, invalidate on every deploy.
    new deploy.BucketDeployment(this, 'Files', {
      sources: [deploy.Source.asset(props.siteDir ?? path.join(path.dirname(fileURLToPath(import.meta.url)), '../../site'))],
      destinationBucket: bucket,
      cacheControl: [deploy.CacheControl.fromString('public, max-age=300')],
      distribution,
      distributionPaths: ['/*'],
    })

    if (props.zone && props.certificate) {
      const zone = route53.HostedZone.fromHostedZoneAttributes(this, 'Zone', props.zone)
      for (const [i, Rec] of [route53.ARecord, route53.AaaaRecord].entries()) {
        new Rec(this, `Alias${i}`, {
          zone,
          recordName: props.zone.domainName,
          target: route53.RecordTarget.fromAlias(new targets.CloudFrontTarget(distribution)),
        })
      }
    }

    new CfnOutput(this, 'Url', { value: `https://${props.zone?.domainName ?? distribution.distributionDomainName}` })
    new CfnOutput(this, 'DistributionId', { value: distribution.distributionId })
  }
}
