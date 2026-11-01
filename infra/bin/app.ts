import { App, Tags } from 'aws-cdk-lib'
import { SiteCertStack, SiteStack } from '../lib/site-stack.js'
import { type PrimaryRefs, StandbyBaseStack } from '../lib/standby-base-stack.js'
import { StrataStack, type StandbyStores } from '../lib/strata-stack.js'

const app = new App()
const ctx = (k: string) => (app.node.tryGetContext(k) as string | undefined) || undefined

// Warm standby (DORA Art. 11-12). Deploy order, one step at a time:
//   1. StrataStandbyBase (keys and buckets in the standby region)
//   2. copy its outputs into dr.standby in cdk.json, deploy Strata (adds the
//      table replica and data replication)
//   3. StrataStandby (API, ledger, anchor, alarms), then `strata-dr init`
// Values are literals in cdk.json: no cross-region references to untangle.
interface DrContext {
  standbyRegion: string
  primary: PrimaryRefs
  standby?: StandbyStores
}
const dr = app.node.tryGetContext('dr') as DrContext | undefined
const account = '067751999967'
const common = {
  alarmEmail: ctx('alarmEmail'),
  tsaUrl: ctx('tsaUrl'),
}

new StrataStack(app, 'Strata', {
  env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: ctx('region') ?? 'eu-west-1' },
  authDomainPrefix: ctx('authDomainPrefix'),
  jiraSecretArn: ctx('jiraSecretArn'),
  ...common,
  replica: dr?.standby && {
    region: dr.standbyRegion,
    indexKeyArn: dr.standby.indexKeyArn,
    dataBucket: dr.standby.dataBucket,
    dataKeyArn: dr.standby.dataKeyArn,
    data: ctx('drDataReplication') !== 'false',
  },
})

if (dr) {
  new StandbyBaseStack(app, 'StrataStandbyBase', { env: { account, region: dr.standbyRegion }, primary: dr.primary })
  if (dr.standby) {
    new StrataStack(app, 'StrataStandby', {
      env: { account, region: dr.standbyRegion },
      ...common,
      standby: { primary: dr.primary, base: dr.standby },
    })
  }
}

// Public showcase site at strata.baschen.is. Cross-region references need a literal account.
const zone = { domainName: 'strata.baschen.is', hostedZoneId: 'Z04336205ZJCG928RTW7', zoneName: 'baschen.is' }
const cert = new SiteCertStack(app, 'StrataSiteCert', { env: { account, region: 'us-east-1' }, crossRegionReferences: true, zone })
new SiteStack(app, 'StrataSite', { env: { account, region: 'eu-west-1' }, crossRegionReferences: true, zone, certificate: cert.certificate })

Tags.of(app).add('project', 'strata')
Tags.of(app).add('repo', 'aws-abaschen/strata')
