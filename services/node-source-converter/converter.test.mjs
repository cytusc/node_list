import test from 'node:test';
import assert from 'node:assert/strict';
import { convertBatch } from './converter.mjs';
const config = overrides => ({type: 'http', server: '192.0.2.1', port: 8080, ...overrides});
const convert = rawConfig => convertBatch({items: [{rawConfig}]}).nodes[0];
test('strict fingerprints ignore only top-level display fields', () => {
  assert.equal(convert(config({name:'one'})).fingerprint,convert(config({name:'two',sub_tag:'x'})).fingerprint);
  for (const change of [{password:'a'},{username:'a'},{tls:true}]) assert.notEqual(convert(config()).fingerprint,convert(config(change)).fingerprint);
  // Unknown fields cannot be silently dropped to fabricate an exportable URI.
  for (const change of [{sni:'x'},{'skip-cert-verify':true},{udp:true},{unknown:{name:'a'}}]) assert.equal(convertBatch({items:[{rawConfig:config(change)}]}).rejected[0].code,'unrepresentable');
});
test('HTTP generation uses URL encoding, TLS and IPv6 round trips', () => {
  const n=convert(config({server:'2001:db8::1',tls:true,username:'a@b',password:'p:/#?'}));
  const uri=new URL(n.generatedLink);
  assert.equal(uri.protocol,'https:');assert.equal(uri.hostname,'[2001:db8::1]');assert.equal(decodeURIComponent(uri.password),'p:/#?');
  assert.equal(n.rawLink,'');assert.equal(n.linkStatus,'generated_verified');
  assert.equal(convertBatch({text:n.generatedLink}).nodes.length,1);
});
test('original URI bytes and unknown options survive, fragments are display only', () => {
  const a='vless://11111111-1111-4111-8111-111111111111@example.com:443?security=tls&type=ws&path=%2Fx&unknown=a#first';
  const n=convertBatch({items:[{rawLink:a},{rawLink:a.replace('#first','#second')},{rawLink:a.replace('unknown=a','unknown=b')}]}).nodes;
  assert.equal(n.length,3);assert.equal(n[0].rawLink,a);assert.equal(n[0].fingerprint,n[1].fingerprint);assert.notEqual(n[0].fingerprint,n[2].fingerprint);
});
test('SOCKS empty authentication is equivalent only for link verification',()=>{
 const n=convert({type:'socks5',server:'192.0.2.1',port:1080});assert.equal(n.linkStatus,'generated_verified');
 const explicit=convert({type:'socks5',server:'192.0.2.1',port:1080,username:'',password:''});
 assert.notEqual(n.fingerprint,explicit.fingerprint);
});
test('missing authentication and conflicting URI are never silently accepted as that config',()=>{
 const bad=convertBatch({items:[{rawConfig:{type:'vless',server:'x.example',port:443}},{rawLink:'invalid'}]});assert.equal(bad.rejected.length,2);
 const n=convertBatch({items:[{rawConfig:config(),rawLink:'http://different.example:8080#raw'}]}).nodes[0];
 assert.equal(n.conflict,true);assert.equal(n.rawLink,'');assert.equal(n.originalEvidence,'http://different.example:8080#raw');assert.ok(n.generatedLink);
});
test('YAML, JSON, base64 subscriptions and partial errors',()=>{
 assert.equal(convertBatch({text:'proxies:\n  - type: http\n    server: 192.0.2.1\n    port: 8080\n'}).nodes.length,1);
 assert.equal(convertBatch({text:JSON.stringify({proxies:[config()]})}).nodes.length,1);
 const text=Buffer.from('http://192.0.2.1:8080#n\ninvalid').toString('base64');const b=convertBatch({text});assert.equal(b.nodes.length,1);assert.equal(b.rejected.length,1);
 assert.throws(()=>convertBatch({items:Array(20001).fill({})}),/input-limit/);
});
test('protocol and transport matrix retains configuration differences',()=>{
 const links=[
  'ss://YWVzLTEyOC1nY206cGFzcw@example.com:443#ss',
  'trojan://pass@example.com:443?sni=s.example#t',
  'vless://11111111-1111-4111-8111-111111111111@example.com:443?security=tls&type=grpc&serviceName=svc#v',
  'vless://11111111-1111-4111-8111-111111111111@example.com:443?security=reality&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=abcd&sni=s.example#r',
  'hysteria2://pass@example.com:443?sni=s.example#h',
  'tuic://11111111-1111-4111-8111-111111111111:pass@example.com:443?sni=s.example#u',
  'anytls://pass@example.com:443?sni=s.example#a',
  'vmess://'+Buffer.from(JSON.stringify({v:'2',ps:'v',add:'example.com',port:'443',id:'11111111-1111-4111-8111-111111111111',aid:'0',net:'ws',path:'/x',tls:'tls',sni:'s.example'})).toString('base64'),
 ];
 const b=convertBatch({items:links.map(rawLink=>({rawLink}))});assert.equal(b.nodes.length,links.length,JSON.stringify(b.rejected));
 for(let i=0;i<links.length;i++)assert.equal(b.nodes[i].rawLink,links[i]);
 for(const suffix of ['?plugin=v2ray-plugin%3Btls','?unknown=1']){
  const b=convertBatch({items:[{rawLink:links[0]},{rawLink:links[0].replace('#ss',suffix+'#ss')}]});assert.equal(b.nodes.length,2);assert.notEqual(b.nodes[0].fingerprint,b.nodes[1].fingerprint);
 }
});

test('raw config extensions cannot hide behind a matching original URI',()=>{
 const result=convertBatch({items:[{rawConfig:config({unknown:'retain'}),rawLink:'http://192.0.2.1:8080'},{rawConfig:config({_ca:'/etc/passwd'})}]});
 assert.equal(result.nodes.length,0);assert.equal(result.rejected.length,2);
});
