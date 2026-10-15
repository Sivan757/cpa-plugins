#!/usr/bin/env node
// Export the OpenCode console credential for use by the CPA plugin.
//
// The free tier accepts only the account session token minted by the OpenCode
// console together with the workspace it is metered against, so a machine that
// cannot sign in needs a copy of both. They live in the CLI's own database:
//
//   ~/.local/share/opencode/opencode.db
//   table credential, row with integration_id='opencode' and active=1
//   value is JSON: { access, refresh, expires, metadata: { orgID, email } }
//
// Read the row where the CLI actually keeps it. The account table in the same
// database holds a different, stale credential; using it yields HTTP 401 or a
// FreeTierError that looks like a fingerprint problem.
//
// Usage: node export.mjs [output-directory]
//
// Write the result with owner-only permissions: it carries a live bearer token.
// Copy it to the server's credential_dir and delete it when it expires.

import { execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, writeFileSync, statSync } from 'node:fs'
import { homedir, hostname } from 'node:os'
import { join } from 'node:path'

const DATABASE = join(homedir(), '.local', 'share', 'opencode', 'opencode.db')
const OUT_DIR = process.argv[2] ?? join(homedir(), '.dsh-cpa')
const OUT_FILE = join(OUT_DIR, 'opencode-account.json')

if (!existsSync(DATABASE)) {
  console.error('No OpenCode database at ' + DATABASE)
  console.error('Sign in with the opencode CLI on this machine first.')
  process.exit(1)
}

// sqlite3 -json keeps the quoting correct and is present on macOS by default.
function query(sql) {
  let raw
  try {
    raw = execFileSync('sqlite3', ['-json', DATABASE, sql], { encoding: 'utf8', timeout: 20000 })
  } catch (err) {
    console.error('Reading ' + DATABASE + ' failed: ' + (err && err.message))
    console.error('Install sqlite3, or copy the database to a machine that has it.')
    process.exit(1)
  }
  const trimmed = raw.trim()
  return trimmed === '' ? [] : JSON.parse(trimmed)
}

const rows = query(
  "select value from credential where integration_id = 'opencode' and active = 1 order by time_updated desc limit 1",
)
if (rows.length === 0) {
  console.error('No active opencode credential in ' + DATABASE)
  console.error('Run: opencode auth login')
  process.exit(1)
}

let credential
try {
  credential = JSON.parse(rows[0].value)
} catch (err) {
  console.error('The stored credential is not JSON: ' + (err && err.message))
  process.exit(1)
}

const access = String(credential.access ?? '')
const refresh = String(credential.refresh ?? '')
const metadata = credential.metadata ?? {}
const orgID = String(metadata.orgID ?? '')

if (!access.startsWith('st_')) {
  console.error('The stored access token is not a console session token: ' + access.slice(0, 6))
  process.exit(1)
}

const document = {
  type: 'opencode',
  access_token: access,
  refresh_token: refresh,
  org_id: orgID,
  // Set in the past so the plugin renews on its first request and adopts a fresh
  // token. The access token above still works until it expires, so a failed
  // renewal does not take the account down.
  expires_at: 1,
  source: 'export-' + hostname(),
}

mkdirSync(OUT_DIR, { recursive: true, mode: 0o700 })
writeFileSync(OUT_FILE, JSON.stringify(document, null, 2) + '\n', { mode: 0o600 })

const expiresAt = Number(credential.expires ?? 0)
const days = expiresAt ? ((expiresAt - Date.now()) / 86400000).toFixed(1) : 'unknown'
console.log('wrote ' + OUT_FILE)
console.log('  access token : st_...' + access.slice(-6) + ' (' + access.length + ' characters)')
console.log('  refresh token: ' + (refresh ? 'rt_...' + refresh.slice(-6) : 'none'))
console.log('  workspace    : ' + (orgID || 'none'))
console.log('  account      : ' + (metadata.email ?? 'unknown'))
console.log('  expires in   : ' + days + ' days')
console.log('  permissions  : ' + (statSync(OUT_FILE).mode & 0o777).toString(8))
console.log('')
console.log('Copy it to the server, into the plugin credential directory:')
console.log('  scp ' + OUT_FILE + ' <server>:/opt/cliproxyapi/data/dsh-cpa/')
