import { App, Tags } from 'aws-cdk-lib'
import { SiteCertStack, SiteStack } from '../lib/site-stack.js'
import { StrataStack } from '../lib/strata-stack.js'

const app = new App()
const ctx = (k: string) => (app.node.tryGetContext(k) as string | undefined) || undefined

new StrataStack(app, 'Strata', {
  env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: ctx('region') ?? 'eu-west-1' },
  authDomainPrefix: ctx('authDomainPrefix'),
  jiraSecretArn: ctx('jiraSecretArn'),
  alarmEmail: ctx('alarmEmail'),
})

// Public showcase site at strata.baschen.is. Cross-region references need a literal account.
const account = '067751999967'
const zone = { domainName: 'strata.baschen.is', hostedZoneId: 'Z04336205ZJCG928RTW7', zoneName: 'baschen.is' }
const cert = new SiteCertStack(app, 'StrataSiteCert', { env: { account, region: 'us-east-1' }, crossRegionReferences: true, zone })
new SiteStack(app, 'StrataSite', { env: { account, region: 'eu-west-1' }, crossRegionReferences: true, zone, certificate: cert.certificate })

Tags.of(app).add('project', 'strata')
Tags.of(app).add('repo', 'aws-abaschen/strata')
