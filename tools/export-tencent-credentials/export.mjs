#!/usr/bin/env node
// Export a WorkBuddy desktop sign-in for use on a machine without the app.
//
// The desktop products keep their access token sealed with AES-256-GCM. The key
// lives in the app's private storage binding, which only the app's own Electron
// binary can reach. This tool asks that binary for the key, decrypts the token,
// and writes the credential document the tencent plugin reads as static-<kind>.json.
//
// Run it on the Mac that is signed in, then copy the output to the server's
// ~/.dsh-cpa/ directory. The output holds a bearer token in the clear, so write
// it with owner-only permissions and delete it when it expires.
//
// Usage: node export.mjs [workbuddy|workbuddy-ai|all]

import { createHash, createDecipheriv } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs'
import { homedir, hostname } from 'node:os'
import { join } from 'node:path'

const PRODUCTS = {
  workbuddy: {
    kind: 'workbuddy',
    label: 'WorkBuddy',
    binary: '/Applications/WorkBuddy.app/Contents/MacOS/Electron',
    authFile: 'workbuddy-desktop.info',
  },
  'workbuddy-ai': {
    kind: 'workbuddy-ai',
    label: 'WorkBuddy AI',
    binary: '/Applications/WorkBuddy AI.app/Contents/MacOS/Electron',
    authFile: 'workbuddy-desktop-ai.info',
  },
}

const AUTH_DIR = join(homedir(), 'Library', 'Application Support', 'CodeBuddyExtension', 'Data', 'Public', 'auth')
const OUT_DIR = join(homedir(), '.dsh-cpa')

// atRestSecret asks the app's Electron binary for its storage secret.
//
// The binding returns JSON, not the bare secret: {"version":1,"atRestSecretKey":"..."}.
// Hashing the whole payload instead of the field yields a wrong key, so the
// field is extracted and the shape is checked.
function atRestSecret(binary) {
  if (!existsSync(binary)) {
    throw new Error(binary + ' is not installed')
  }
  const script = 'process.stdout.write(String(process._linkedBinding("electron_browser_workbuddy_storage").loggerGet()))'
  const out = execFileSync(binary, ['-e', script], {
    env: { ...process.env, ELECTRON_RUN_AS_NODE: '1' },
    encoding: 'utf8',
    timeout: 20000,
  }).trim()
  if (!out) {
    throw new Error('the app returned an empty at-rest payload')
  }
  let payload
  try {
    payload = JSON.parse(out)
  } catch {
    throw new Error('the at-rest payload is not JSON: ' + out.slice(0, 80))
  }
  if (payload.version !== 1 || !payload.atRestSecretKey) {
    throw new Error('unsupported at-rest payload (version ' + payload.version + ')')
  }
  const decoded = Buffer.from(payload.atRestSecretKey, 'base64')
  if (decoded.length !== 32) {
    throw new Error('the at-rest secret is not a canonical 32-byte base64 value')
  }
  return payload.atRestSecretKey
}

// deriveKey mirrors the plugin: the secret string is hashed, not its base64 bytes.
function deriveKey(secret) {
  const key = createHash('sha256').update(secret, 'utf8').digest()
  const keyId = createHash('sha256').update(key).digest('hex').slice(0, 16)
  return { key, keyId }
}

function lengthPrefixed(value) {
  const bytes = Buffer.from(value, 'utf8')
  const length = Buffer.alloc(4)
  length.writeUInt32BE(bytes.length, 0)
  return Buffer.concat([length, bytes])
}

// buildAAD reproduces the app's authenticated-context builder byte for byte.
function buildAAD(keyId, suite) {
  const suiteBytes = Buffer.alloc(4)
  suiteBytes.writeUInt32BE(suite, 0)
  return Buffer.concat([
    Buffer.from('WB-AAD\u0000', 'utf8'),
    Buffer.from([0x01]),
    lengthPrefixed('WBEV1'),
    lengthPrefixed('sym-v1'),
    suiteBytes,
    lengthPrefixed(keyId),
    Buffer.from([0x02, 0x00, 0x00]),
  ])
}

// open decrypts one sealed field. A plaintext field is returned unchanged.
function open(field, key, keyId, label) {
  if (!field || typeof field !== 'object') {
    return typeof field === 'string' ? field : ''
  }
  if (field.$wbEncrypted !== 1) {
    return typeof field.envelope === 'string' ? field.envelope : ''
  }
  const envelope = JSON.parse(Buffer.from(field.envelope, 'base64').toString('utf8'))
  if (envelope.suite !== 1) {
    throw new Error(label + ': unsupported envelope suite ' + envelope.suite)
  }
  if (envelope.keyId !== keyId) {
    throw new Error(label + ': envelope key id does not match the installed app key')
  }
  const decipher = createDecipheriv('aes-256-gcm', key, Buffer.from(envelope.nonce, 'base64'))
  decipher.setAAD(buildAAD(envelope.keyId, envelope.suite))
  decipher.setAuthTag(Buffer.from(envelope.authTag, 'base64'))
  return Buffer.concat([
    decipher.update(Buffer.from(envelope.ciphertext, 'base64')),
    decipher.final(),
  ]).toString('utf8')
}

function exportProduct(product) {
  const spec = PRODUCTS[product]
  const authPath = join(AUTH_DIR, spec.authFile)
  if (!existsSync(authPath)) {
    throw new Error('no sign-in file at ' + authPath + '; sign in to ' + spec.label + ' first')
  }
  const doc = JSON.parse(readFileSync(authPath, 'utf8'))
  const secret = atRestSecret(spec.binary)
  const { key, keyId } = deriveKey(secret)

  const accessToken = open(doc.auth?.accessToken, key, keyId, 'accessToken')
  if (!accessToken) {
    throw new Error('accessToken decrypted to an empty string')
  }
  let refreshToken = ''
  try {
    refreshToken = open(doc.auth?.refreshToken, key, keyId, 'refreshToken')
  } catch {
    refreshToken = ''
  }

  const record = {
    kind: spec.kind,
    mode: 'static',
    access_token: accessToken,
    refresh_token: refreshToken,
    uid: doc.account?.uid ?? '',
    enterprise_id: doc.account?.enterpriseId ?? '',
    domain: doc.auth?.domain ?? doc.account?.domain ?? doc.account?.sso?.domain ?? '',
    expires_at: doc.auth?.expiresAt ?? 0,
    app_version: doc.appVersion ?? '',
    cli_version: doc.cliVersion ?? '',
    source_machine: hostname(),
  }

  mkdirSync(OUT_DIR, { recursive: true, mode: 0o700 })
  const outPath = join(OUT_DIR, 'static-' + spec.kind + '.json')
  writeFileSync(outPath, JSON.stringify(record, null, 2) + '\n', { mode: 0o600 })

  const days = record.expires_at ? ((record.expires_at - Date.now()) / 86400000).toFixed(1) : 'unknown'
  console.log(spec.label + ': wrote ' + outPath)
  console.log('  token length ' + accessToken.length + ', refresh length ' + refreshToken.length)
  console.log('  expires in ' + days + ' days')
  console.log('  copy it to the server with:')
  console.log('    scp ' + outPath + ' tencent-2c4g:~/.dsh-cpa/')
}

const requested = process.argv[2] ?? 'all'
const targets = requested === 'all' ? Object.keys(PRODUCTS) : [requested]
if (!targets.every((name) => PRODUCTS[name])) {
  console.error('usage: node export.mjs [workbuddy|workbuddy-ai|all]')
  process.exit(2)
}
let failures = 0
for (const target of targets) {
  try {
    exportProduct(target)
  } catch (err) {
    failures += 1
    console.error(PRODUCTS[target].label + ': ' + err.message)
  }
}
process.exit(failures === 0 ? 0 : 1)
