import { test } from 'node:test'
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
