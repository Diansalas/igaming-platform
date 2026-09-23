// Unit tests for the staging CloudFront viewer allowlist function
// (deploy/aws/modules/edge/viewer-ip-allowlist.js.tftpl, ADR 0086).
// Renders the template exactly as Terraform's templatefile() would (the
// only interpolation is ${allowed_cidrs_json} -> jsonencode(list)) and
// evaluates the handler. Run: node --test deploy/aws/tests/edge-allowlist.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
const template = readFileSync(join(here, '../modules/edge/viewer-ip-allowlist.js.tftpl'), 'utf8');

function load(cidrs) {
  const placeholder = '${allowed_cidrs_json}';
  assert.equal(template.split(placeholder).length, 2, 'template must contain the placeholder exactly once');
  assert.ok(!template.replace(placeholder, '').includes('${'), 'no other Terraform interpolation may appear in the template');
  assert.ok(!template.includes('%{'), 'no Terraform template directives may appear in the template');
  const code = template.replace(placeholder, JSON.stringify(cidrs));
  const ctx = {};
  vm.createContext(ctx);
  vm.runInContext(code, ctx);
  return (ip) => ctx.handler({ viewer: { ip }, request: { uri: '/v1/me', method: 'GET' } });
}

const allowed = (res) => res && res.uri === '/v1/me';
const denied = (res) => res && res.statusCode === 403;

test('exact /32 match is allowed, neighbours are denied', () => {
  const h = load(['203.0.113.10/32']);
  assert.ok(allowed(h('203.0.113.10')));
  assert.ok(denied(h('203.0.113.11')));
  assert.ok(denied(h('203.0.113.9')));
});

test('/24 range boundaries', () => {
  const h = load(['198.51.100.0/24']);
  assert.ok(allowed(h('198.51.100.0')));
  assert.ok(allowed(h('198.51.100.255')));
  assert.ok(denied(h('198.51.101.0')));
  assert.ok(denied(h('198.51.99.255')));
});

test('addresses >= 128.0.0.0 work (no signed 32-bit bitwise bugs)', () => {
  const h = load(['203.0.113.0/24', '255.255.255.254/31']);
  assert.ok(allowed(h('203.0.113.200')));
  assert.ok(allowed(h('255.255.255.255')));
  assert.ok(denied(h('255.255.255.253')));
});

test('multiple CIDRs: any match allows', () => {
  const h = load(['10.1.0.0/16', '203.0.113.10/32']);
  assert.ok(allowed(h('10.1.200.3')));
  assert.ok(allowed(h('203.0.113.10')));
  assert.ok(denied(h('10.2.0.1')));
});

test('non-canonical CIDR base still matches its block', () => {
  const h = load(['198.51.100.77/24']);
  assert.ok(allowed(h('198.51.100.1')));
});

test('fails closed on IPv6, malformed or missing addresses', () => {
  const h = load(['203.0.113.10/32']);
  for (const ip of ['2001:db8::1', '::ffff:203.0.113.10', '203.0.113', '203.0.113.10.1', '203.0.113.256', '203.0.113.-1', ' 203.0.113.10', '', undefined, null]) {
    assert.ok(denied(h(ip)), `expected 403 for ${String(ip)}`);
  }
});

test('an empty allowlist denies everyone (Terraform validation also forbids it)', () => {
  const h = load([]);
  assert.ok(denied(h('203.0.113.10')));
});
