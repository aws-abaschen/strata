import { App, Tags } from 'aws-cdk-lib'
import { StrataStack } from '../lib/strata-stack.js'

const app = new App()
const ctx = (k: string) => (app.node.tryGetContext(k) as string | undefined) || undefined

new StrataStack(app, 'Strata', {
  env: { account: process.env.CDK_DEFAULT_ACCOUNT, region: ctx('region') ?? 'eu-west-1' },
  authDomainPrefix: ctx('authDomainPrefix'),
  jiraSecretArn: ctx('jiraSecretArn'),
})

Tags.of(app).add('project', 'strata')
Tags.of(app).add('repo', 'aws-abaschen/strata')
