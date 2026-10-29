<?php
declare(strict_types=1);

// 仅在专用测试库执行。创建独立随机 schema，结束时清理，不依赖或修改已有夹具。
require __DIR__ . '/bootstrap.php';

use app\controller\OpenApiController;
use app\service\{ApiError, IpClassification, OpenApiAuth, OpenNodes, OpenNodesQuery};
use support\{Db, Request};

if (!str_contains((string)env('PGDATABASE', ''), 'test')) {
    fwrite(STDERR, "请将 PGDATABASE 指向名称含 test 的专用测试数据库。\n");
    exit(2);
}
$passed = 0;
function ensure(bool $condition, string $message): void {
    global $passed;
    if (!$condition) throw new RuntimeException($message);
    $passed++;
}
function expect(mixed $expected, mixed $actual, string $message): void {
    ensure($expected === $actual, $message . ': expected ' . json_encode($expected) . ', got ' . json_encode($actual));
}
function seedNode(string $id, ?array $attributes = [], string $country = 'HK'): string {
    static $serial = 0;
    $ip = '198.51.100.' . ++$serial;
    $node = ['name' => '测试节点 ' . $id, 'type' => 'vless', 'server' => 'example.com', 'port' => 443,
        'country' => $country, 'speed' => 999 - $serial, 'rawLink' => 'vless://test@example.com:443?path=%2F#' . $id,
        'generatedLink' => 'http://fallback:secret@example.com:80', 'rawConfig' => ['password' => 'private-config-sentinel'],
        'provider' => 'private-source-sentinel'];
    Db::table('node_explorer_node_registry')->insert(['identity' => $id, 'available' => true, 'node' => json_encode($node)]);
    Db::insert("INSERT INTO node_explorer_connectivity (identity,checked_at,payload)
        SELECT identity,now(),jsonb_build_object('reachable',true,'incarnation',incarnation,
          'configDigest',md5((node - 'rawLink' - 'generatedLink')::text))
        FROM node_explorer_node_registry WHERE identity=?", [$id]);
    Db::table('node_explorer_exits')->insert(['identity' => $id, 'ip' => $ip, 'checked_at' => date('c'),
        'payload' => json_encode(['ip' => $ip, 'country' => $country, 'verified' => true])]);
    if ($attributes !== null) {
        $value = array_replace([
            'ip' => $ip, 'status' => 'success', 'country' => $country, 'score' => 5,
            'humanPercent' => 80, 'botPercent' => 20, 'source' => '原生IP',
            'classification' => ['ip' => $ip, 'version' => (int)config('api.voting_version', 8), 'state' => 'unknown',
                'types' => [['provider' => 'one', 'usageType' => '住宅IP']]],
        ], $attributes);
        Db::table('node_explorer_ip_attributes')->insert(['ip' => $ip, 'checked_at' => date('c'), 'payload' => json_encode($value)]);
    }
    return $ip;
}
function replaceAttributes(string $ip, array $patch): void {
    Db::update('UPDATE node_explorer_ip_attributes SET payload=payload || ?::jsonb WHERE ip=?', [json_encode($patch), $ip]);
}
function replaceClassification(string $ip, array $types, ?int $version = null, ?string $classificationIp = null): void {
    replaceAttributes($ip, ['classification' => ['ip' => $classificationIp ?? $ip,
        'version' => $version ?? (int)config('api.voting_version', 8), 'state' => 'residential', 'types' => $types]]);
}

$schema = 'open_api_test_' . getmypid() . '_' . bin2hex(random_bytes(4));
Db::statement('CREATE SCHEMA "' . $schema . '"');
try {
    Db::statement('SET search_path TO "' . $schema . '"');
    Db::unprepared(file_get_contents(dirname(__DIR__, 2) . '/backend/internal/database/schema.sql'));
    $service = new OpenNodes();
    seedNode('a-zero', ['score' => 0, 'humanPercent' => 20, 'botPercent' => 80, 'source' => '广播IP']);
    seedNode('b-native');
    seedNode('c-native');
    seedNode('d-broadcast', ['source' => '广播IP']);
    seedNode('e-source-unknown', ['source' => null]);
    seedNode('f-bot', ['humanPercent' => 20, 'botPercent' => 80]);
    seedNode('g-equal', ['humanPercent' => 50, 'botPercent' => 50]);
    seedNode('h-ratio-missing', ['botPercent' => null]);
    seedNode('i-risk-six', ['score' => 6, 'humanPercent' => 99, 'botPercent' => 1]);
    seedNode('j-risk-high', ['score' => 99]);
    seedNode('k-risk-missing', ['score' => null]);
    seedNode('l-risk-invalid', ['score' => -1]);
    $hosting = seedNode('m-hosting', ['score' => 0]);
    replaceClassification($hosting, [['provider' => 'one', 'usageType' => '机房IP']]);
    $business = seedNode('n-business', ['score' => 1]);
    replaceClassification($business, [['provider' => 'one', 'companyType' => '商业宽带']]);
    $mobile = seedNode('o-mobile', ['score' => 2]);
    replaceClassification($mobile, [['provider' => 'one', 'usageType' => '移动网络']]);
    seedNode('p-type-unknown', ['score' => 0, 'classification' => null, 'attributes' => ['住宅IP']]);
    seedNode('q-stale', null);
    Db::update("UPDATE node_explorer_exits SET pinned_attributes=?::jsonb WHERE identity='q-stale'", [json_encode([
        'ip' => '203.0.113.200', 'status' => 'success', 'score' => 0, 'humanPercent' => 100, 'botPercent' => 0, 'source' => '原生IP',
        'classification' => ['ip' => '203.0.113.200', 'version' => 8, 'types' => [['provider' => 'one', 'usageType' => '住宅IP']]],
    ])]);
    seedNode('r-no-attributes', null);
    $expected = ['a-zero', 'b-native', 'c-native', 'd-broadcast', 'e-source-unknown', 'f-bot', 'g-equal', 'h-ratio-missing', 'i-risk-six', 'j-risk-high', 'k-risk-missing', 'l-risk-invalid', 'm-hosting', 'n-business', 'o-mobile', 'p-type-unknown', 'q-stale', 'r-no-attributes'];

    $page1 = $service->nodes(new OpenNodesQuery('HK'));
    $page2 = $service->nodes(new OpenNodesQuery('HK', 2));
    expect(array_slice($expected, 0, 10), array_column($page1['nodes'], 'id'), 'Strict four-level order on page one');
    expect(array_slice($expected, 10), array_column($page2['nodes'], 'id'), 'Full sort before pagination, page two');
    expect(18, $page1['total'], 'Total contains all eligible nodes');
    expect(10, $page1['page_size'], 'Fixed page size');
    expect(true, $page1['has_more'], 'First page has more');
    expect(false, $page2['has_more'], 'Last page has no more');
    expect($page1, $service->nodes(new OpenNodesQuery('HK')), 'Stable identity order across requests');
    $past = $service->nodes(new OpenNodesQuery('HK', 2147483647));
    expect([], $past['nodes'], 'Out-of-range page is empty');
    expect(2147483647, $past['page'], 'Out-of-range page is not clamped');
    expect(false, $past['has_more'], 'Out-of-range page has no more');
    expect(0, $service->nodes(new OpenNodesQuery('US'))['total'], 'Valid country with no nodes is empty');

    $all = array_column(array_merge($page1['nodes'], $page2['nodes']), null, 'id');
    expect(0, $all['a-zero']['risk_score'], 'Zero is valid low risk');
    expect(true, $all['b-native']['residential'], 'Residential is derived from types despite stored state unknown');
    expect(false, $all['m-hosting']['residential'], 'Hosting is known non-residential');
    expect(false, $all['n-business']['residential'], 'Business is known non-residential');
    expect(null, $all['p-type-unknown']['residential'], 'Bare residential label does not grant residential ranking');
    expect(false, $all['g-equal']['human_preferred'], 'Equal traffic ratio is not human-preferred');
    expect(null, $all['h-ratio-missing']['human_preferred'], 'One missing traffic ratio is unknown');
    expect(null, $all['k-risk-missing']['risk_score'], 'Missing risk remains null');
    expect(null, $all['l-risk-invalid']['risk_score'], 'Invalid risk remains null');
    expect(null, $all['e-source-unknown']['native'], 'Missing source is not native');
    expect(false, $all['d-broadcast']['native'], 'Broadcast is known non-native');
    foreach (['q-stale', 'r-no-attributes'] as $id) {
        foreach (['residential', 'risk_score', 'human_preferred', 'native'] as $field) {
            expect(null, $all[$id][$field], 'Historical or absent attribute is unknown: ' . $id . '/' . $field);
        }
    }
    $controller = new OpenApiController();
    $response = $controller->nodes(new Request("GET /api/open/v1/nodes?country=hk HTTP/1.1\r\n\r\n"), $service);
    $body = json_decode($response->rawBody(), true);
    expect('OK', $body['code'], 'Success envelope');
    expect('HK', $body['data']['country'], 'Controller normalizes country');
    expect('no-store', $response->getHeader('Cache-Control'), 'Credential response cannot be cached');
    expect('vless://test@example.com:443?path=%2F#b-native', $all['b-native']['link'], 'Raw link preserved exactly');
    ensure(!str_contains($response->rawBody(), 'private-config-sentinel') && !str_contains($response->rawBody(), 'private-source-sentinel') && !str_contains($response->rawBody(), 'fallback:secret'), 'No raw config, source or unused fallback link exposed');

    // SQL 类型归一必须与现有 PHP 入口一致，包括 provider 去重和非住宅分类。
    $parity = seedNode('type-parity', [], 'CA');
    foreach ([
        [],
        [['provider' => 'a', 'usageType' => '住宅IP']],
        [['provider' => 'a', 'usageType' => '机房IP']],
        [['provider' => 'a', 'companyType' => '商业宽带']],
        [['provider' => 'a', 'usageType' => '机房IP', 'companyType' => '住宅IP']],
        [['provider' => 'a', 'usageType' => '住宅IP'], ['provider' => 'a', 'usageType' => '机房IP'], ['provider' => 'b', 'usageType' => '机房IP']],
        [['provider' => 'a', 'usageType' => '机房IP'], ['provider' => 'b', 'usageType' => '商业宽带']],
        [['provider' => 'a', 'usageType' => '住宅IP'], ['provider' => 'b', 'usageType' => '机房IP'], ['provider' => 'c', 'usageType' => '机房IP']],
        [['provider' => 'a', 'usageType' => '住宅IP'], ['provider' => 'b', 'usageType' => '商业宽带'], ['provider' => 'c', 'usageType' => '机房IP']],
        [['provider' => 'a', 'usageType' => '移动网络']],
        [['provider' => 'a', 'usageType' => '教育IP'], ['provider' => 'b', 'usageType' => '移动网络']],
    ] as $types) {
        replaceClassification($parity, $types);
        $node = $service->nodes(new OpenNodesQuery('CA'))['nodes'][0];
        $expectedType = IpClassification::summaryType($types);
        expect($expectedType === '类型未知' ? null : $expectedType, $node['network_type'], 'SQL/PHP classification parity');
    }
    replaceClassification($parity, [['provider' => 'a', 'usageType' => '住宅IP']], 0);
    expect(null, $service->nodes(new OpenNodesQuery('CA'))['nodes'][0]['residential'], 'Old voting version is unknown');
    replaceClassification($parity, [['provider' => 'a', 'usageType' => '住宅IP']], classificationIp: '203.0.113.99');
    expect(null, $service->nodes(new OpenNodesQuery('CA'))['nodes'][0]['residential'], 'Classification from a different IP is unknown');
    foreach ([['score' => '0', 'humanPercent' => -1, 'botPercent' => 110, 'source' => 'ippure'],
              ['score' => 101, 'humanPercent' => '90', 'botPercent' => 10, 'source' => null],
              ['score' => null, 'humanPercent' => 90, 'botPercent' => 10, 'trafficRatioStatus' => 'error']] as $patch) {
        replaceAttributes($parity, $patch);
        $node = $service->nodes(new OpenNodesQuery('CA'))['nodes'][0];
        expect(null, $node['risk_score'], 'Malformed/missing risk is unknown');
        expect(null, $node['human_preferred'], 'Malformed/failed traffic ratio is unknown');
        expect(null, $node['native'], 'Unrecognized/missing native source is unknown');
    }

    // 同一 IP 的 pinned fallback 即使仍有旧住宅结论，也不获得排序优势。
    Db::update('UPDATE node_explorer_exits SET pinned_attributes=(SELECT payload FROM node_explorer_ip_attributes WHERE ip=?) WHERE identity=?', [$parity, 'type-parity']);
    Db::delete('DELETE FROM node_explorer_ip_attributes WHERE ip=?', [$parity]);
    expect(null, $service->nodes(new OpenNodesQuery('CA'))['nodes'][0]['network_type'], 'Pinned fallback marked stale is ignored even for the same IP');

    seedNode('jp-generated', [], 'jp');
    Db::update("UPDATE node_explorer_node_registry SET node=node || '{\"rawLink\":\"  \"}'::jsonb WHERE identity='jp-generated'");
    expect('http://fallback:secret@example.com:80', $service->nodes(new OpenNodesQuery('JP'))['nodes'][0]['link'], 'Blank raw link falls back to stored generated link');
    seedNode('no-link', [], 'US');
    Db::update("UPDATE node_explorer_node_registry SET node=node-'rawLink'-'generatedLink' WHERE identity='no-link'");
    expect(0, $service->nodes(new OpenNodesQuery('US'))['total'], 'No link means not externally usable');
    seedNode('bad-country', [], '');
    expect([
        ['code' => 'CA', 'name' => '加拿大', 'node_count' => 1],
        ['code' => 'HK', 'name' => app\service\Country::name('HK'), 'node_count' => 18],
        ['code' => 'JP', 'name' => '日本', 'node_count' => 1],
    ], $service->countries(), 'Country counts share eligibility, normalize case, exclude no-link and unknown country');
    foreach ($service->countries() as $country) {
        expect($country['node_count'], $service->nodes(new OpenNodesQuery($country['code']))['total'], 'Country/list total agrees');
    }
    foreach ([
        "UPDATE node_explorer_node_registry SET available=false WHERE identity='jp-generated'",
        "UPDATE node_explorer_connectivity SET payload=payload || '{\"reachable\":false}'::jsonb WHERE identity='jp-generated'",
        "UPDATE node_explorer_connectivity SET checked_at=now()-interval '7 hours' WHERE identity='jp-generated'",
        "UPDATE node_explorer_connectivity SET payload=payload || '{\"incarnation\":\"old\"}'::jsonb WHERE identity='jp-generated'",
        "UPDATE node_explorer_node_registry SET node=node || '{\"port\":8443}'::jsonb WHERE identity='jp-generated'",
        "UPDATE node_explorer_exits SET payload=payload || '{\"verified\":false}'::jsonb WHERE identity='jp-generated'",
        "UPDATE node_explorer_exits SET checked_at=now()-interval '7 hours' WHERE identity='jp-generated'",
    ] as $mutation) {
        Db::beginTransaction();
        try {
            Db::statement($mutation);
            expect(0, $service->nodes(new OpenNodesQuery('JP'))['total'], 'Invalid current liveness/exit is excluded');
            ensure(!in_array('JP', array_column($service->countries(), 'code'), true), 'Invalid node also disappears from countries');
        } finally { Db::rollBack(); }
    }

    // 实际 PostgreSQL 防重放及跨请求存储；表权限丢失时必须拒绝访问。
    putenv('OPEN_API_APP_ID=test-client');
    putenv('OPEN_API_SECRET=0123456789abcdef0123456789abcdef');
    Webman\Config::load(dirname(__DIR__) . '/config', ['route']);
    $timestamp = (string)time();
    $nonce = bin2hex(random_bytes(16));
    $signature = hash_hmac('sha256', implode("\n", ['GET', '/api/open/v1/countries', '', hash('sha256', ''), $timestamp, $nonce]), '0123456789abcdef0123456789abcdef');
    $raw = "GET /api/open/v1/countries HTTP/1.1\r\nHost: localhost\r\nX-App-Id: test-client\r\nX-Timestamp: $timestamp\r\nX-Nonce: $nonce\r\nX-Signature: $signature\r\n\r\n";
    OpenApiAuth::authenticate(new Request($raw));
    expect(1, Db::table('node_explorer_open_api_nonces')->count(), 'PostgreSQL stores nonce');
    try {
        OpenApiAuth::authenticate(new Request($raw));
        throw new RuntimeException('Replay was accepted');
    } catch (ApiError $e) { expect('REPLAY_DETECTED', $e->errorCode, 'PostgreSQL atomically rejects replay'); }
    Db::table('node_explorer_open_api_nonces')->update(['expires_at' => time() - 1]);
    OpenApiAuth::authenticate(new Request($raw));
    expect(1, Db::table('node_explorer_open_api_nonces')->count(), 'Expired nonce can be reclaimed in PostgreSQL');

    echo "OK $passed open API PostgreSQL assertions\n";
} finally {
    Db::statement('SET search_path TO public');
    Db::statement('DROP SCHEMA "' . $schema . '" CASCADE');
}
