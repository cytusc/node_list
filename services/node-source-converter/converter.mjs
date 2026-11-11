import { createRequire } from 'node:module';
import { createHash } from 'node:crypto';
import { readFileSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import canonicalize from 'canonicalize';
import { parseDocument } from 'yaml';

export const VERSION = 'strict-v1/sub-store-2.44.2';
const MAX_ITEMS = 20000;
const MAX_BYTES = 32 * 1024 * 1024;
const artifact = new URL('./vendor/proxy-utils.esm.mjs', import.meta.url);
if (createHash('sha256').update(readFileSync(artifact)).digest('hex') !== 'ed870a8728056663acee57629472261a83d0277ba7ff3b8ec8b8f0fe98d52847') throw Error('converter-integrity');
const scratch = mkdtempSync(join(tmpdir(), 'source-parser-'));
process.env.SUB_STORE_DATA_BASE_PATH = scratch;
process.on('exit', () => rmSync(scratch, { recursive: true, force: true }));
globalThis.require = createRequire(import.meta.url);
// The upstream module logs raw URIs. Only our bounded JSON response may leave this process.
for (const key of ['log', 'info', 'warn', 'error', 'debug']) console[key] = () => {};
const { parse, produce } = await import('./vendor/proxy-utils.esm.mjs');
const clone = value => structuredClone(value);
const hash = value => createHash('sha256').update(value).digest('hex');
const clean = config => Object.fromEntries(Object.entries(config).filter(([key]) => !['name', 'sub_tag'].includes(key)));
const fail = code => { throw Error(code); };
function one(text) {
  if (/^https?:\/\//.test(text)) {
    const uri = new URL(text);
    if (uri.search || (uri.pathname && uri.pathname !== '/')) fail('unsupported');
    const proxy = {type: 'http', server: uri.hostname.replace(/^\[|\]$/g, ''), port: Number(uri.port || (uri.protocol === 'https:' ? 443 : 80))};
    if (uri.protocol === 'https:') proxy.tls = true;
    if (uri.username) proxy.username = decodeURIComponent(uri.username);
    if (uri.password) proxy.password = decodeURIComponent(uri.password);
    return proxy;
  }
  const nodes = parse(text);
  if (!Array.isArray(nodes) || nodes.length !== 1) fail('invalid');
  return nodes[0];
}
function mihomo(proxy) {
  const output = produce([clone(proxy)], 'mihomo', 'internal');
  if (!Array.isArray(output) || output.length !== 1) fail('invalid');
  return clean(output[0]);
}
function safeFields(config) {
  if (!config || typeof config !== 'object') return;
  for (const [key, value] of Object.entries(config)) {
    if (key.startsWith('_')) fail('unsupported');
    safeFields(value);
  }
}
function endpoint(config) {
  if (!config || typeof config !== 'object' || Array.isArray(config)) fail('invalid');
  if (typeof config.server !== 'string' || !config.server.trim() || /[\s\x00-\x1f]/.test(config.server)) fail('incomplete');
  const port = Number(config.port);
  if (!Number.isInteger(port) || port < 1 || port > 65535) fail('incomplete');
  const type = config.type;
  if (type === 'ssh' && (!config.username || !(config.password || config['private-key']))) fail('incomplete');
  const required = ({ss: ['cipher', 'password'], vmess: ['uuid'], vless: ['uuid'], trojan: ['password'], hysteria2: ['password'], tuic: ['uuid', 'password'], anytls: ['password'], snell: ['psk'], wireguard: ['private-key', 'public-key']})[type];
  if (!['http', 'socks5', 'ss', 'vmess', 'vless', 'trojan', 'hysteria', 'hysteria2', 'tuic', 'anytls', 'wireguard', 'ssh', 'snell'].includes(type)) fail('unsupported');
  if (required?.some(key => typeof config[key] !== 'string' || !config[key])) fail('incomplete');
}
function basicHTTP(config) {
  if (Object.keys(clean(config)).some(key => !['type', 'server', 'port', 'username', 'password', 'tls'].includes(key))) fail('unrepresentable');
  if ('tls' in config && typeof config.tls !== 'boolean') fail('invalid');
  const url = new URL(`${config.tls ? 'https' : 'http'}://${config.server.includes(':') && !config.server.startsWith('[') ? `[${config.server}]` : config.server}:${config.port}`);
  url.username = config.username ?? '';
  url.password = config.password ?? '';
  url.hash = config.name ?? '';
  const back = new URL(url.href);
  if (back.hostname.replace(/^\[|\]$/g, '') !== config.server.replace(/^\[|\]$/g, '') || Number(back.port || (back.protocol === 'https:' ? 443 : 80)) !== Number(config.port) || decodeURIComponent(back.username) !== (config.username ?? '') || decodeURIComponent(back.password) !== (config.password ?? '')) fail('unrepresentable');
  return url.href;
}
function roundtripConfig(config) {
  const result = clone(config);
  // Sub-Store emits empty SOCKS authentication on URI round trips; absent and empty both mean no auth.
  if (result.type === 'socks5') for (const key of ['username', 'password']) if (result[key] === '') delete result[key];
  return result;
}
function generated(config, parsed) {
  if (config.type === 'http') return basicHTTP(config);
  const uri = produce([clone(parsed)], 'URI');
  if (typeof uri !== 'string' || !uri.trim() || /[\r\n]/.test(uri.trim())) fail('unrepresentable');
  const back = one(uri);
  if (canonicalize(roundtripConfig(mihomo(parsed))) !== canonicalize(roundtripConfig(mihomo(back)))) fail('unrepresentable');
  // Unknown source fields must also survive. Normalized library output alone is insufficient.
  const before = clean(config), after = mihomo(back);
  for (const [key, value] of Object.entries(before)) {
    if (canonicalize(value) !== canonicalize(after[key] ?? null)) fail('unrepresentable');
  }
  return uri.trim();
}
function inputIdentity(link) {
  // Preserve all unknown URI options as identity material; only the display fragment is removed.
  if (link.startsWith('vmess://')) return link;
  try { const uri = new URL(link); uri.hash = ''; return uri.href; } catch { return link; }
}
function convert(item) {
  const original = typeof item.rawLink === 'string' ? item.rawLink : '';
  let config = item.rawConfig && Object.keys(item.rawConfig).length ? clone(item.rawConfig) : null;
  let parsed, uriParsed, conflict = false;
  if (original) {
    try { uriParsed = one(original); } catch { if (!config) fail('invalid'); }
  }
  if (config) {
    safeFields(config);
    parsed = one(JSON.stringify({ proxies: [config] }));
    if (uriParsed) {
      const back = mihomo(uriParsed);
      conflict = canonicalize(mihomo(parsed)) !== canonicalize(back) || Object.entries(clean(config)).some(([key, value]) => canonicalize(value) !== canonicalize(back[key] ?? null));
    }
  } else if (uriParsed) {
    parsed = uriParsed;
    config = mihomo(parsed);
  } else fail('incomplete');
  if (parsed._ca) fail('unsupported');
  endpoint(config);
  // Any URI-only input keeps its opaque URI identity: conversion may discard extensions.
  const material = { config: clean(config) };
  if (original) material.uri = inputIdentity(original);
  const canonical = canonicalize(material);
  const fingerprint = hash(VERSION + '\0' + canonical);
  let generatedLink = '', rawLink = original;
  // Conflicting/unparseable originals remain evidence, never a usable link for this config.
  if (!original || !uriParsed || conflict) {
    generatedLink = generated(config, parsed);
    rawLink = '';
  }
  return { fingerprint, canonical, config, name: item.name || config.name || '', rawLink, generatedLink,
    originalEvidence: original, linkStatus: rawLink ? 'original' : 'generated_verified', conflict: Boolean(original && (!uriParsed || conflict)) };
}
function itemsFromText(text) {
  if (typeof text !== 'string' || Buffer.byteLength(text) > MAX_BYTES) fail('input-limit');
  let document;
  try { const parsed = parseDocument(text); if (parsed.errors.length) throw Error('invalid'); document = parsed.toJS({ maxAliasCount: 20 }); } catch { /* URI subscription */ }
  if (document && Array.isArray(document.proxies)) return document.proxies.map(rawConfig => ({ rawConfig }));
  let lines = text.trim().split(/\r?\n/).filter(line => line.trim() && !line.trim().startsWith('#'));
  if (lines.length === 1 && !lines[0].includes('://')) {
    const decoded = Buffer.from(text.trim(), 'base64').toString('utf8');
    if (!decoded.includes('://')) fail('invalid');
    lines = decoded.split(/\r?\n/).filter(line => line.trim());
  }
  return lines.map(rawLink => ({ rawLink }));
}
export function convertBatch(request) {
  const items = request.items ?? itemsFromText(request.text);
  if (!Array.isArray(items) || items.length > MAX_ITEMS) fail('input-limit');
  const nodes = [], rejected = [];
  items.forEach((item, index) => {
    try { nodes.push({ index, ...convert(item) }); }
    catch (error) { rejected.push({ index, code: ['invalid','incomplete','unsupported','unrepresentable'].includes(error.message) ? error.message : 'invalid' }); }
  });
  return { version: VERSION, nodes, rejected, total: items.length };
}
if (process.argv[1] && import.meta.url === new URL(process.argv[1], 'file:').href) {
  try {
    let size = 0, chunks = [];
    for await (const chunk of process.stdin) { size += chunk.length; if (size > MAX_BYTES) fail('input-limit'); chunks.push(chunk); }
    const result = convertBatch(JSON.parse(Buffer.concat(chunks).toString('utf8')));
    const output = JSON.stringify(result);
    if (Buffer.byteLength(output) > 64 * 1024 * 1024) fail('output-limit');
    process.stdout.write(output);
  } catch { process.stderr.write('conversion-failed\n'); process.exitCode = 1; }
}
