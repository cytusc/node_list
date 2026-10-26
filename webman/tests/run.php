<?php
declare(strict_types=1);

/**
 * 离线单元测试（不依赖 PHPUnit，也不连数据库）。
 *
 * 覆盖「契约最容易漂移」的五处：
 *   1. 参数校验（ResultsQueryParser）—— 400 的边界与文案
 *   2. 排序键/布尔筛选枚举（SortKey / Feature）
 *   3. 节点 JSON 规范化（NodePresenter）—— 字段顺序、零值、空数组→null、凭证剥离
 *   4. facets 分桶与标签（FacetKind）
 *   5. 查询构造器单表查询 + PHP 处理管线（过滤/排序/分页/facets/preferred）
 *
 * 用法：php tests/run.php
 * 退出码 0 = 全绿；非 0 = 有失败（可直接接 CI）。
 */

require __DIR__ . '/bootstrap.php';

use app\service\ApiError;
use app\service\Country;
use app\service\Heartbeat;
use app\service\JsonResponse;
use app\service\LiveResults;
use app\service\NodePresenter;
use app\service\ResultsQueryParser;
use app\service\ResultsStatus;
use app\service\enum\Direction;
use app\service\enum\FacetKind;
use app\service\enum\Feature;
use app\service\enum\SortKey;

$passed = 0;
$failed = [];

/**
 * 断言辅助。
 */
function ok(bool $condition, string $message): void
{
    global $passed, $failed;
    if ($condition) {
        $passed++;
        return;
    }
    $failed[] = $message;
}

function same(mixed $expected, mixed $actual, string $message): void
{
    global $passed, $failed;
    if ($expected === $actual) {
        $passed++;
        return;
    }
    $failed[] = $message . ' — 期望 ' . var_export($expected, true) . '，实际 ' . var_export($actual, true);
}

/** 断言调用抛 ApiError 且状态码/文案符合预期。 */
function rejects(callable $fn, int $status, string $message): void
{
    global $passed, $failed;
    try {
        $fn();
    } catch (ApiError $e) {
        if ($e->status === $status) {
            $passed++;
            return;
        }
        $failed[] = $message . ' — 状态码期望 ' . $status . '，实际 ' . $e->status;
        return;
    } catch (Throwable $e) {
        $failed[] = $message . ' — 抛出意外的 ' . $e::class;
        return;
    }
    $failed[] = $message . ' — 没有抛出 ApiError';
}

// ---------------------------------------------------------------------------
// 1. 参数校验
// ---------------------------------------------------------------------------

same(
    null,
    ResultsQueryParser::fromQuery([]),
    '无 page/limit 时视为不分页（返回 null）'
);

$q = ResultsQueryParser::fromQuery(['page' => '2', 'limit' => '20']);
same(2, $q?->page, 'page 解析');
same(20, $q?->limit, 'limit 解析');
same(Direction::Desc, $q?->direction, 'direction 缺省为 desc');

// page/limit 空串走默认值（Go 的 positiveInt 语义）
$q = ResultsQueryParser::fromQuery(['page' => '', 'limit' => '']);
same(1, $q?->page, 'page 空串 → 1');
same(50, $q?->limit, 'limit 空串 → 50');

// +3 在 Go 的 strconv.Atoi 下合法，这里必须同样通过
$q = ResultsQueryParser::fromQuery(['page' => '+3']);
same(3, $q?->page, 'page 接受前导正号');

rejects(fn () => ResultsQueryParser::fromQuery(['page' => '0']), 400, 'page=0 拒绝');
rejects(fn () => ResultsQueryParser::fromQuery(['page' => '-1']), 400, 'page 负数拒绝');
rejects(fn () => ResultsQueryParser::fromQuery(['page' => '1.5']), 400, 'page 小数拒绝');
rejects(fn () => ResultsQueryParser::fromQuery(['limit' => '201']), 400, 'limit 超过 200 拒绝');
// 上一行故意用 rejects 会失败，这里用正向断言兜回来
$q = ResultsQueryParser::fromQuery(['limit' => '200']);
same(200, $q?->limit, 'limit=200 允许');

rejects(fn () => ResultsQueryParser::fromQuery(['page' => '1', 'sort' => 'bogus']), 400, '未知 sort 拒绝');
rejects(fn () => ResultsQueryParser::fromQuery(['page' => '1', 'direction' => 'up']), 400, '非法 direction 拒绝');
rejects(fn () => ResultsQueryParser::fromQuery(['page' => '1', 'minSpeed' => '-1']), 400, '负 minSpeed 拒绝');
rejects(fn () => ResultsQueryParser::fromQuery(['page' => '1', 'minSpeed' => 'abc']), 400, '非数值 minSpeed 拒绝');
rejects(fn () => ResultsQueryParser::fromQuery(['page' => '1', 'features' => 'tls,ssh']), 400, '未知 feature 拒绝');

$q = ResultsQueryParser::fromQuery(['page' => '1', 'sort' => 'preferred']);
ok($q?->preferred === true && $q?->sort === null, 'sort=preferred 单独标记，不落到具体列');

$q = ResultsQueryParser::fromQuery(['page' => '1', 'sort' => 'speed', 'direction' => 'asc']);
same(SortKey::Speed, $q?->sort, 'sort=speed 解析为枚举');
same(Direction::Asc, $q?->direction, 'direction=asc 解析');

$q = ResultsQueryParser::fromQuery([
    'page' => '1', 'country' => 'US,JP', 'type' => '', 'features' => 'tls,udp', 'q' => '  美国  ',
]);
same(['US', 'JP'], $q?->countries, 'country 逗号切分');
same([], $q?->types, 'type 空串视为无筛选');
same([Feature::Tls, Feature::Udp], $q?->features, 'features 解析为枚举列表');
same('美国', $q?->search, 'q 做 trim（不加小写影响中文）');

$q = ResultsQueryParser::fromQuery(['page' => '1', 'country' => 'US,,JP,']);
same(['US', 'JP'], $q?->countries, '逗号切分丢弃空项');

// ---------------------------------------------------------------------------
// 2. 枚举
// ---------------------------------------------------------------------------

same(
    ['country', 'name', 'type', 'speed', 'asnOrganization', 'ipSource', 'ipAttributes', 'ippure', 'trafficRatio'],
    array_map(static fn (SortKey $k): string => $k->value, SortKey::cases()),
    'SortKey 取值顺序与 Go 的 domain.ResultSortKeys 一致'
);
ok(SortKey::Speed->isNumeric() && !SortKey::Name->isNumeric(), '数值型排序键判定');
ok(str_contains(SortKey::Name->sqlExpression(), 'regexp_replace'), '名称排序剥离国旗 emoji');
same(['tls', 'reality', 'udp'], Feature::values(), 'Feature 取值集合');
same('TLS', Feature::Tls->label(), 'Feature 标签');
same(
    ['country', 'type', 'network', 'ipSource', 'ipAttributes', 'features', 'media'],
    array_keys(FacetKind::emptyBuckets()),
    'facet 空桶顺序稳定'
);

// ---------------------------------------------------------------------------
// 3. 节点规范化
// ---------------------------------------------------------------------------

$node = NodePresenter::node([
    'name' => '🇺🇸 美国 01',
    'baseName' => '美国 01',
    'type' => 'ss',
    'port' => 443,          // 数字 → 字符串
    'tls' => true,
    'udp' => false,         // 非 omitempty → 保留 false
    'speed' => 5242880.0,   // 整数值浮点 → 整数
    'country' => 'US',
    'media' => [],          // present + omitempty + 空容器 → 非 nil 空切片 → []
    'details' => [['k' => '加密', 'v' => 'aes', 'extra' => 'ignored']],
    'lifecycle' => ['status' => 'active'],
    'rawLink' => 'ss://secret',   // 必须被剥离
    'generatedLink' => 'http://generated-secret:password@example.com:80',
    'rawConfig' => ['server' => 'x'],
    'addedAt' => '2026-10-17T00:00:00Z',
    'checkedAt' => '2026-10-18T00:00:00Z',
]);

same('2026-10-17T00:00:00Z', $node['addedAt'] ?? null, '添加时间保留在节点响应中');
same('443', $node['port'] ?? null, 'port 数字字符串化');
same(5242880, $node['speed'] ?? null, 'speed 整数值不输出 .0');
same(false, $node['udp'] ?? null, '非 omitempty 的 false 保留');
same([], $node['media'] ?? null, '空 media 输出 []（原载荷明确给了空数组，Go 解码后是非 nil 空切片）');
// 对比：键存在但值为 null 时才是 null（nil slice）。注意不能用 `??` 取值——
// 它把「键存在且为 null」和「键不存在」看成一样，等于把要断言的东西抹掉了。
$nullMedia = NodePresenter::node(['name' => 'x', 'media' => null]);
ok(array_key_exists('media', $nullMedia) && $nullMedia['media'] === null, 'media 显式 null 输出 null');
same([['k' => '加密', 'v' => 'aes']], $node['details'] ?? null, 'details 只保留 k/v');
same(['status' => 'active'], $node['lifecycle'] ?? null, 'lifecycle 原样输出');
ok(!array_key_exists('rawLink', $node), 'rawLink 被剥离');
ok(!array_key_exists('generatedLink', $node), '生成链接凭据被剥离');
ok(!array_key_exists('rawConfig', $node), 'rawConfig 被剥离');

// 键顺序必须与 Go 的结构体声明顺序一致（前端不依赖，但快照指纹依赖）
$order = array_keys($node);
same('name', $order[0], '首字段是 name');
same('speed', $order[array_search('tls', $order, true) + 2] ?? null, 'tls 之后依次是 reality、udp，再往后是 network');
// 全字段节点：直接固化「键顺序 = Go 结构体声明顺序」这一契约（去掉被剥离的 raw*）。
$full = NodePresenter::node([
    'name' => 'n', 'baseName' => 'b', 'type' => 't', 'server' => 's', 'port' => 1, 'sni' => 'x',
    'tls' => true, 'reality' => true, 'udp' => true, 'network' => 'tcp', 'speed' => 1.5,
    'country' => 'US', 'claimedCountry' => 'JP', 'ip' => '1.1.1.1', 'ipRisk' => 'low',
    'media' => [['platform' => 'Netflix', 'tag' => '解锁']], 'subTag' => 'tag',
    'details' => [['k' => 'a', 'v' => 'b']],
    'ippure' => ['checkedAt' => 'x', 'attempts' => 1, 'status' => 'success'],
    'connectivity' => ['reachable' => true], 'exit' => ['ip' => '2.2.2.2'],
    'lifecycle' => ['status' => 'active'], 'snapshotId' => 7,
    'checkedAt' => 'c', 'speedTest' => true, 'mediaCheck' => true,
]);
same(
    ['name', 'baseName', 'type', 'server', 'port', 'sni', 'tls', 'reality', 'udp', 'network',
     'speed', 'country', 'claimedCountry', 'ip', 'ipRisk', 'media', 'subTag', 'details',
     'ippure', 'connectivity', 'exit', 'lifecycle', 'snapshotId', 'checkedAt', 'speedTest', 'mediaCheck'],
    array_keys($full),
    '节点键顺序与 domain.Node 声明顺序一致'
);

// reality 缺失 → 不输出（Go 的 omitempty + 未 present → 删除）
ok(!array_key_exists('reality', $node), '缺失的 realism 不凭空出现');
// baseline：payload 里给了 false 就要能看见
$node2 = NodePresenter::node(['name' => 'x', 'reality' => false, 'tls' => false, 'udp' => false, 'speed' => 0]);
ok(array_key_exists('reality', $node2) && $node2['reality'] === false, 'present 的 false 保留');
same(0, $node2['speed'] ?? null, 'speed=0 保留');

// ippure 规范 + 嵌套 classification
$node3 = NodePresenter::node([
    'speed' => 0, 'tls' => false, 'udp' => false,
    'ippure' => [
        'checkedAt' => '2026-10-18T00:00:00Z',
        'attempts' => 0,
        'status' => 'success',
        'ip' => '203.0.113.10',
        'attributes' => ['residential'],
        'score' => 85.0,
        'classification' => [
            'version' => 8, 'ip' => '203.0.113.10', 'checkedAt' => '2026-10-18T00:00:00Z',
            'state' => 'residential',
            'types' => [['provider' => 'ippure', 'usageType' => '住宅IP']],
            'evidence' => [[
                'provider' => 'ippure', 'checkedAt' => '2026-10-18T00:00:00Z', 'ip' => '203.0.113.10',
                'hosting' => false, 'vpn' => false, 'proxy' => false, 'outcome' => 'supports-residential',
            ]],
        ],
    ],
]);
same(0, $node3['ippure']['attempts'] ?? null, 'ippure.attempts=0 保留');
same(85, $node3['ippure']['score'] ?? null, 'ippure.score 整数值');
same('residential', $node3['ippure']['classification']['state'] ?? null, 'classification.state');
same(false, $node3['ippure']['classification']['evidence'][0]['hosting'] ?? null, 'hosting=false 是有效证据');
same(
    ['version', 'ip', 'checkedAt', 'state'],
    array_slice(array_keys($node3['ippure']['classification']), 0, 4),
    'classification 键顺序（version/ip/checkedAt/state）'
);

// ---------------------------------------------------------------------------
// 4. facets
// ---------------------------------------------------------------------------

$facets = LiveResults::facets([
    ['kind' => 'country', 'value' => 'US', 'count' => 3],
    ['kind' => 'country', 'value' => 'unknown', 'count' => 1],
    ['kind' => 'type', 'value' => 'ss', 'count' => 4],
    ['kind' => 'network', 'value' => 'tcp', 'count' => 4],
    ['kind' => 'features', 'value' => 'tls', 'count' => 4],
    ['kind' => 'media', 'value' => 'Netflix', 'count' => 2],
]);
same('美国', $facets['country'][0]['label'], 'country 标签走中文名');
same('未知地区', $facets['country'][1]['label'], 'unknown 地区标签');
same('SS', $facets['type'][0]['label'], 'type 标签大写');
same('TCP', $facets['network'][0]['label'], 'network 标签大写');
same('Netflix', $facets['media'][0]['label'], 'media 标签原样');
same(3, count($facets['features']), 'features 固定三项');
same(['tls', 'reality', 'udp'], array_column($facets['features'], 'value'), 'features 顺序固定');
same(0, $facets['features'][1]['count'], '缺失的 feature 计数补 0');
same('Reality', $facets['features'][1]['label'], 'feature 标签');
same([], $facets['ipSource'], '空桶是空数组而不是 null');

// ---------------------------------------------------------------------------
// 5. 查询构造器（单表、无 JOIN）+ PHP 处理管线
// ---------------------------------------------------------------------------

$live = new LiveResults();

// 5a. 查询基于两张视图、且是单表查询（不含 JOIN）
same('node_explorer_live_facts', LiveResults::factsTable(), 'facts 查询走实时视图');
same('node_explorer_live_nodes', LiveResults::payloadsTable(), 'payload 查询走节点视图');
// 构造器单表查询，默认不含 JOIN；下面用合成数据验证「JOIN 已被移除、逻辑在 PHP 完成」。

// 5b. 合成数据驱动 PHP 处理管线（过滤 / 排序 / 分页 / facets / preferred）。
// 字段刻意覆盖 Go 契约里的边缘情形：住宅+新鲜证据（优选）、speed=0、IPv6 unsupported、无测速。
$now = new DateTimeImmutable('2026-10-18T00:10:00Z');
$t8 = '2026-10-18T00:08:00Z'; // now-2min，落在优选窗内

function nl_fact(array $p): stdClass
{
    $o = new stdClass();
    foreach ($p as $k => $v) {
        $o->$k = $v;
    }
    return $o;
}

$raw = [
    nl_fact([
        'identity' => 'id-us', 'has_original_link' => true, 'snapshot_id' => 10, 'node_index' => 0, 'country' => 'US', 'region' => 'US',
        'name' => '美国 01', 'base_name' => '美国 01', 'server' => 's', 'sni' => '', 'node_type' => 'ss',
        'protocol' => 'ss', 'network' => 'tcp', 'speed' => 1048576, 'tls' => true, 'reality' => true, 'udp' => true,
        'speed_test' => true, 'media_check' => true, 'checked_at' => '2026-10-18T00:00:00Z',
        'attributes_ip' => '203.0.113.10', 'ip_status' => 'success', 'ip_source' => 'ippure',
        'asn_organization' => 'ACME', 'ip_score' => '85', 'human_percent' => '90',
        'classification' => json_encode(['state' => 'residential', 'version' => 8, 'ip' => '203.0.113.10', 'checkedAt' => $t8, 'evidence' => [['checkedAt' => $t8]]]),
        'ip_attributes' => json_encode(['residential']),
        'reachable' => true, 'connectivity_error' => '', 'connectivity_at' => $t8,
        'reported_exit_ip' => '203.0.113.10', 'exit_error' => '', 'exit_at' => $t8,
        'media' => json_encode(['Netflix']),
    ]),
    nl_fact([
        'identity' => 'id-jp', 'has_original_link' => 'f', 'snapshot_id' => 10, 'node_index' => 1, 'country' => 'JP', 'region' => 'JP',
        'name' => '日本 10', 'base_name' => '日本 10', 'server' => 's', 'sni' => '', 'node_type' => 'ss',
        'protocol' => 'ss', 'network' => 'tcp', 'speed' => 2097152, 'tls' => true, 'reality' => true, 'udp' => false,
        'speed_test' => true, 'media_check' => true, 'checked_at' => '2026-10-18T00:00:00Z',
        'attributes_ip' => '198.51.100.10', 'ip_status' => 'success', 'ip_source' => 'ippure',
        'asn_organization' => 'ACME', 'ip_score' => '90', 'human_percent' => '88',
        'classification' => json_encode(['state' => 'residential', 'version' => 8, 'ip' => '198.51.100.10', 'checkedAt' => $t8, 'evidence' => [['checkedAt' => $t8]]]),
        'ip_attributes' => json_encode(['residential']),
        'reachable' => true, 'connectivity_error' => '', 'connectivity_at' => $t8,
        'reported_exit_ip' => '198.51.100.10', 'exit_error' => '', 'exit_at' => $t8,
        'media' => json_encode(['Netflix', 'YouTube']),
    ]),
    nl_fact([
        'identity' => 'id-de', 'snapshot_id' => 10, 'node_index' => 2, 'country' => 'DE', 'region' => 'DE',
        'name' => '德国 02', 'base_name' => '德国 02', 'server' => 's', 'sni' => '', 'node_type' => 'vmess',
        'protocol' => 'vmess', 'network' => 'tcp', 'speed' => 0, 'tls' => false, 'reality' => false, 'udp' => true,
        'speed_test' => true, 'media_check' => true, 'checked_at' => '2026-10-18T00:00:00Z',
        'attributes_ip' => '', 'ip_status' => '', 'ip_source' => '', 'asn_organization' => '',
        'ip_score' => null, 'human_percent' => null,
        'classification' => null, 'ip_attributes' => json_encode([]),
        'reachable' => false, 'connectivity_error' => '', 'connectivity_at' => '',
        'reported_exit_ip' => '', 'exit_error' => '', 'exit_at' => '', 'media' => json_encode([]),
    ]),
    nl_fact([
        'identity' => 'id-sg', 'snapshot_id' => 10, 'node_index' => 3, 'country' => 'SG', 'region' => 'SG',
        'name' => '新加坡 01', 'base_name' => '新加坡 01', 'server' => 's', 'sni' => '', 'node_type' => 'ss',
        'protocol' => 'ss', 'network' => 'tcp', 'speed' => 0, 'tls' => true, 'reality' => false, 'udp' => true,
        'speed_test' => true, 'media_check' => true, 'checked_at' => '2026-10-18T00:00:00Z',
        'attributes_ip' => '', 'ip_status' => 'unsupported', 'ip_source' => '', 'asn_organization' => '',
        'ip_score' => null, 'human_percent' => null,
        'classification' => null, 'ip_attributes' => json_encode([]),
        'reachable' => false, 'connectivity_error' => 'no route', 'connectivity_at' => '',
        'reported_exit_ip' => '', 'exit_error' => '', 'exit_at' => '', 'media' => json_encode(['Netflix']),
    ]),
    nl_fact([
        'identity' => 'id-hk', 'snapshot_id' => 10, 'node_index' => 4, 'country' => 'HK', 'region' => 'HK',
        'name' => '香港 03', 'base_name' => '香港 03', 'server' => 's', 'sni' => '', 'node_type' => 'ss',
        'protocol' => 'ss', 'network' => 'tcp', 'speed' => 0, 'tls' => true, 'reality' => false, 'udp' => false,
        'speed_test' => false, 'media_check' => false, 'checked_at' => '2026-10-18T00:00:00Z',
        'attributes_ip' => '', 'ip_status' => '', 'ip_source' => '', 'asn_organization' => '',
        'ip_score' => null, 'human_percent' => null,
        'classification' => null, 'ip_attributes' => json_encode([]),
        'reachable' => false, 'connectivity_error' => '', 'connectivity_at' => '',
        'reported_exit_ip' => '', 'exit_error' => '', 'exit_at' => '', 'media' => json_encode([]),
    ]),
];

$rows = $live->normalizeFacts($raw);
$payloads = [];
foreach ($rows as $r) {
    $payloads[$r['identity']] = (object)['name' => $r['name'], 'port' => '443', 'lifecycle' => ['status' => 'active'], 'checkedAt' => '2026-10-18T00:00:00Z', 'tls' => true];
}

// 过滤
same(1, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'country' => 'US']), $now)[1], 'php 筛选 country=US');
same(1, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'type' => 'vmess']), $now)[1], 'php 筛选 type=vmess');
same(4, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'features' => 'tls']), $now)[1], 'php 筛选 features=tls');
same(2, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'features' => 'reality']), $now)[1], 'php 筛选 features=reality');
same(2, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'features' => 'tls,reality']), $now)[1], 'php 多 features 是 AND');
same(1, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'q' => '美国']), $now)[1], 'php 搜索「美国」命中 US');
same(1, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'q' => '香港']), $now)[1], 'php 搜索「香港」命中 HK');
same(3, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'media' => 'Netflix']), $now)[1], 'php 筛选 media=Netflix');
same(1, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'media' => 'Netflix,YouTube']), $now)[1], 'php 筛选 media 多选是包含判定');
same(2, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'ipSource' => 'ippure']), $now)[1], 'php 筛选 ipSource=ippure');
same(2, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'ipAttributes' => 'residential']), $now)[1], 'php 筛 ipAttributes=residential');
same(2, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'minSpeed' => '1']), $now)[1], 'php minSpeed=1 按字节换算过滤');
same(2, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'minSpeed' => '2']), $now)[1], 'php minSpeed=2 两条高速节点都过');
same(1, $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '50', 'minSpeed' => '1536']), $now)[1], 'php minSpeed=1536（阈值 1572864 字节）只留日本');

// 分页
[$p1, $t1] = $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '2']), $now);
same(2, count($p1), 'limit=2 返回 2 条');
same(5, $t1, 'total 是过滤后总数');
[$p3] = $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '3', 'limit' => '2']), $now);
same(1, count($p3), '第 3 页只有 1 条');
[, $to, $pg] = $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '99', 'limit' => '2']), $now);
same(3, $pg, '页码越界夹到最后一页');

// 优选排序：US/JP 都 residential+新鲜证据，speed 高者（日本 10）排第一
[$pref] = $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '1', 'sort' => 'preferred']), $now);
same('日本 10', $pref[0]['name'], '优选排序把速度最高的住宅节点排第一');

// 排序：speed 升序，速度为 0 的德国 02 排第一
[$bySpeedAsc] = $live->resolvePage($rows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '5', 'sort' => 'speed', 'direction' => 'asc']), $now);
same('德国 02', $bySpeedAsc[0]['name'], 'speed asc 第一条是速度 0 的节点');

// facets / summary（基于全量，不受筛选影响）
$body = $live->buildBody($rows, $rows, $payloads, count($rows), 0, 0, $now);
same(true, $body['nodes'][0]['hasOriginalLink'], '数据库有原始链接时允许复制');
same(false, $body['nodes'][1]['hasOriginalLink'], '数据库无原始链接时禁用复制');
same(false, $body['nodes'][2]['hasOriginalLink'], '缺失原始链接状态时禁用复制');
same(5, $body['totalNodes'], 'summary.totalNodes 是全量节点数');
same(5, $body['summary']['countries'], 'summary.countries 计不重复非空地区');
same(2097152, $body['summary']['maxSpeed'], 'summary.maxSpeed');
same(1572864, $body['summary']['median'], 'summary.median 偶数样本取中间两值平均');
same(['country', 'type', 'network', 'ipSource', 'ipAttributes', 'features', 'media'], array_keys($body['facets']), 'facet 分桶齐全');
same(5, count($body['facets']['country']), 'country 桶含 5 个地区，每桶计数 1');
same(['tls', 'reality', 'udp'], array_column($body['facets']['features'], 'value'), 'features 固定三项且顺序稳定');
same(4, $body['facets']['features'][0]['count'], 'tls 计数');
same(2, $body['facets']['features'][1]['count'], 'reality 计数');

// 回归：详情按 types 派生住宅标签时，筛选、facet 和优选也必须使用同一结论。
$legacy = clone $raw[0];
$legacy->classification = json_encode([
    'state' => 'datacenter', 'version' => 8, 'ip' => $legacy->attributes_ip,
    'checkedAt' => $t8,
    'types' => [['provider' => 'ipinfo', 'usageType' => '住宅IP']],
    'evidence' => [['checkedAt' => $t8]],
]);
$legacy->ip_attributes = json_encode(['机房IP', '原生IP']);
foreach (['success', 'error'] as $riskStatus) {
    $legacy->ip_status = $riskStatus;
    $derivedRows = $live->normalizeFacts([$legacy, $raw[2]]);
    $derivedQuery = ResultsQueryParser::fromQuery(['page' => '1', 'ipAttributes' => '住宅IP']);
    same(1, $live->resolvePage($derivedRows, $derivedQuery, $now)[1], "{$riskStatus}：住宅筛选不能漏掉详情中的住宅节点");
    $derivedPayload = (object)['ippure' => (object)[
        'ip' => $legacy->attributes_ip, 'status' => $riskStatus,
        'classification' => json_decode($legacy->classification),
        'attributes' => json_decode($legacy->ip_attributes),
    ]];
    $presented = NodePresenter::node($derivedPayload)['ippure'];
    same($presented['attributes'], $derivedRows[0]['ip_attributes'], '筛选标签与详情一致');
    same($presented['classification']['state'], $derivedRows[0]['classification']->state, '优选分类与详情一致');
    $derivedBody = $live->buildBody($derivedRows, $derivedRows, [], 2, 0, 0, $now);
    $derivedFacets = array_column($derivedBody['facets']['ipAttributes'], 'count', 'value');
    same(1, $derivedFacets['住宅IP'] ?? 0, 'facet 统计包含派生住宅标签');
    ok(!isset($derivedFacets['机房IP']), 'facet 不保留过时机房标签');
    // 德国节点名称靠前；只有住宅优选生效，旧标签节点才会排在它前面。
    $noSpeedRows = array_map(static function (array $r): array {
        $r['speed_test'] = false;
        return $r;
    }, $derivedRows);
    [$derivedPage] = $live->resolvePage($noSpeedRows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '1']), $now);
    same('id-us', $derivedPage[0]['identity'], '派生住宅节点在首页优先展示');
}
$mismatched = clone $legacy;
$mismatched->attributes_ip = '203.0.113.99';
same(['机房IP', '原生IP'], $live->normalizeFacts([$mismatched])[0]['ip_attributes'], '分类 IP 不一致时不派生住宅标签');
same('datacenter', json_decode($legacy->classification)->state, '派生不修改原始分类');
$hosting = clone $legacy;
$hosting->classification = (object)[
    'state' => 'residential', 'version' => 8, 'ip' => $hosting->attributes_ip,
    'checkedAt' => $t8, 'types' => [(object)['provider' => 'ipinfo', 'usageType' => '机房IP']],
];
$hosting->ip_attributes = ['住宅IP'];
$hostingRows = $live->normalizeFacts([$hosting]);
same(0, $live->resolvePage($hostingRows, $derivedQuery, $now)[1], '过时住宅标签不能继续命中住宅筛选');
same(['机房IP'], $hostingRows[0]['ip_attributes'], '当前类型证据改判机房');
same('residential', $hosting->classification->state, '规范化不修改输入对象');
$expired = clone $legacy;
$expiredClassification = json_decode($expired->classification);
$expiredClassification->checkedAt = '2026-10-16T00:08:00Z';
$expired->classification = $expiredClassification;
$expired->speed_test = false;
$expiredOther = clone $raw[2];
$expiredOther->speed_test = false;
$expiredRows = $live->normalizeFacts([$expired, $expiredOther]);
[$expiredPage] = $live->resolvePage($expiredRows, ResultsQueryParser::fromQuery(['page' => '1', 'limit' => '1']), $now);
same('id-de', $expiredPage[0]['identity'], '标签重算不能让过期住宅证据获得优选');
same($expiredClassification->checkedAt, $expiredRows[0]['classification']->checkedAt, '标签重算保留证据时间');

// ---------------------------------------------------------------------------
// 6. 国家名 & 时间格式
// ---------------------------------------------------------------------------

same('美国', Country::name('US'), 'Country::name 正常取值');
same('未知地区', Country::name(''), '空代码 → 未知地区');
same('未知地区', Country::name('unknown'), 'unknown → 未知地区');
same('XX', Country::name('XX'), '未收录代码原样返回');
ok(in_array('US', Country::codesMatching('美国'), true), 'codesMatching 命中 US');

// 时间渲染跟着「本地时区当前偏移」走（对齐 Go 的做法），所以这里显式切时区，
// 避免断言结果随机器时区漂移；顺带把 +08 与零值哨兵两条分支都钉住。
date_default_timezone_set('UTC');
same('2026-10-18T00:00:00Z', ResultsStatus::rfc3339('2026-10-18 00:00:00+00'), 'RFC3339 秒精度');
same('0001-01-01T00:00:00Z', ResultsStatus::rfc3339(''), '解析失败退回 Go 的零值字面量');
// 心跳节流：reset 后立即该写，写入后一个间隔内不再写。
// （实际写库走集成测试；这里只钉节流逻辑，不需要数据库。）
Heartbeat::reset();
ok(Heartbeat::shouldBeat(), 'reset 后应该写心跳');
Heartbeat::beat('api-test-noop'); // 写库失败只记日志，不影响节流状态
ok(!Heartbeat::shouldBeat(), '写入后一个间隔内不再写');
Heartbeat::reset();
ok(Heartbeat::shouldBeat(), 'reset 后恢复可写');
date_default_timezone_set('Asia/Shanghai');
same(
    '2026-10-18T08:00:00+08:00',
    ResultsStatus::rfc3339('2026-10-18 00:00:00+00'),
    '非 UTC 部署下按本地当前偏移渲染（与 Go 的 time.Local 一致）'
);
same(
    '0000-12-31T23:54:17+08:00',
    ResultsStatus::rfc3339('0001-01-01 00:00:00+08:05:43'),
    '零值哨兵按 +08:00 渲染，不套历史 LMT（+08:05:43）'
);
date_default_timezone_set('UTC');

same(3, JsonResponse::number(3.0), '整数值浮点转 int');
same(3.5, JsonResponse::number(3.5), '非整数保持浮点');
same(0, JsonResponse::number(null), 'null → 0');

// ---------------------------------------------------------------------------
// 结果
// ---------------------------------------------------------------------------

require __DIR__ . '/monitor.php';

$total = $passed + count($failed);
if ($failed !== []) {
    fwrite(STDERR, "FAILED " . count($failed) . "/{$total}\n");
    foreach ($failed as $message) {
        fwrite(STDERR, '  - ' . $message . "\n");
    }
    exit(1);
}
echo "OK {$total} assertions\n";
