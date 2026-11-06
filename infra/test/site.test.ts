import { test } from 'node:test'
import assert from 'node:assert/strict'
import { App } from 'aws-cdk-lib'
import { Match, Template } from 'aws-cdk-lib/assertions'
import { SiteStack } from '../lib/site-stack.js'

const template = Template.fromStack(new SiteStack(new App(), 'Site', { env: { account: '111111111111', region: 'eu-west-1' } }))

test('site is private behind CloudFront with a strict CSP', () => {
  template.hasResourceProperties('AWS::S3::Bucket', {
    PublicAccessBlockConfiguration: Match.objectLike({ BlockPublicAcls: true, RestrictPublicBuckets: true }),
  })
  template.hasResourceProperties('AWS::CloudFront::ResponseHeadersPolicy', {
    ResponseHeadersPolicyConfig: Match.objectLike({
      SecurityHeadersConfig: Match.objectLike({
        ContentSecurityPolicy: Match.objectLike({ ContentSecurityPolicy: Match.stringLikeRegexp("^default-src 'none'") }),
      }),
    }),
  })
})

test('missing pages answer 404, not the home page', () => {
  template.hasResourceProperties('AWS::CloudFront::Distribution', {
    DistributionConfig: Match.objectLike({
      CustomErrorResponses: Match.arrayWith([Match.objectLike({ ErrorCode: 404, ResponseCode: 404, ResponsePagePath: '/404.html' })]),
    }),
  })
})

test('only /figures/* may run scripts, from this origin, framed by this site', () => {
  const policies = Object.values(template.findResources('AWS::CloudFront::ResponseHeadersPolicy')).map(
    (r) => r.Properties.ResponseHeadersPolicyConfig.SecurityHeadersConfig,
  )
  const csps = policies.map((c) => c.ContentSecurityPolicy.ContentSecurityPolicy as string)
  const page = csps.find((c) => c.includes("frame-ancestors 'none'"))!
  const figure = csps.find((c) => c.includes("frame-ancestors 'self'"))!
  assert.ok(page && !page.includes('script-src') && page.includes("frame-src 'self'"))
  assert.ok(figure.includes("script-src 'self'") && !/unsafe-(inline|eval)/.test(figure))
  template.hasResourceProperties('AWS::CloudFront::Distribution', {
    DistributionConfig: Match.objectLike({
      CacheBehaviors: Match.arrayWith([Match.objectLike({ PathPattern: '/figures/*' })]),
    }),
  })
})
