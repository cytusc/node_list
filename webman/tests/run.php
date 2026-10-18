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
 *   5. SQL 拼装的参数顺序（Condition / LiveResults::build）
 *
 * 用法：php tests/run.php
 * 退出码 0 = 全绿；非 0 = 有失败（可直接接 CI）。
 */

require __DIR__ . '/bootstrap.php';

use app\service\ApiError;
use app\service\Condition;
use app\service\Country;
use app\service\Heartbeat;
use app\service\JsonResponse;
use app\service\LiveResults;
use app\service\NodePresenter;
use app\service\ResultsQueryParser;
use app\service\ServiceBoard;
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
    'rawConfig' => ['server' => 'x'],
    'checkedAt' => '2026-10-18T00:00:00Z',
]);

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
// 5. SQL 拼装
// ---------------------------------------------------------------------------

$merged = Condition::merge([
    Condition::inList('region', ['US', 'JP']),
    Condition::raw('tls'),
]);
same(' WHERE region = ANY(ARRAY[?,?]::text[]) AND tls', $merged[0], '条件合并为 WHERE 子句');
same(['US', 'JP'], $merged[1], '条件参数按顺序展开');

$live = new LiveResults();
[$sql, $bindings] = $live->build(ResultsQueryParser::fromQuery([
    'page' => '1', 'limit' => '10', 'country' => 'US', 'features' => 'tls', 'minSpeed' => '1',
]));
ok(str_contains($sql, 'node_explorer_live_facts'), 'SQL 走实时视图');
ok(str_contains($sql, 'MATERIALIZED'), '过滤/排序收敛在 MATERIALIZED CTE 内');
ok(str_contains($sql, 'page_candidates p JOIN node_explorer_live_nodes'), '只在选中页之后回连视图');
same(6, count($bindings), '绑定参数个数 = 1(country) + 1(minSpeed) + 4(分页)；features 内联列名不占位');
same('US', $bindings[0], '第一个参数是 country 值');
same(1024.0, $bindings[1], 'minSpeed 换算成字节速度（1Mbps × 1024）');
same(1, $bindings[2], '分页参数排在过滤参数之后');

[$sql2, $bindings2] = $live->build(null);
same([], $bindings2, '不分页时没有绑定参数');
ok(str_contains($sql2, 'SELECT 0::bigint AS page,0::bigint AS page_limit'), '不分页时 page/limit 回 0');
ok(!str_contains($sql2, 'LIMIT (SELECT page_limit'), '不分页时没有 LIMIT');

[$sql3] = $live->build(ResultsQueryParser::fromQuery(['page' => '1', 'sort' => 'ippure', 'direction' => 'asc']));
ok(str_contains($sql3, "CASE WHEN ip_status='success' THEN coalesce(ip_score,0) END ASC NULLS LAST"), '按 ippure 升序');

// 搜索条件：中文地区名 → 代码集合
[$sql4, $bindings4] = $live->build(ResultsQueryParser::fromQuery(['page' => '1', 'q' => '美国']));
ok(str_contains($sql4, 'strpos(lower(v),?)'), '搜索命中基础字段');
ok(in_array('US', $bindings4, true), '搜索「美国」展开出 US 代码');
same('美国', $bindings4[0], '搜索词先绑定，再绑定地区代码');

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
same('2026-10-18T00:00:00Z', ServiceBoard::rfc3339('2026-10-18 00:00:00+00'), 'RFC3339 秒精度');
same('0001-01-01T00:00:00Z', ServiceBoard::rfc3339(''), '解析失败退回 Go 的零值字面量');
same(
    '2026-10-18T00:00:00.5Z',
    ServiceBoard::goTime('2026-10-18 00:00:00.500000+00'),
    'RFC3339Nano 裁掉尾随零'
);

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
    ServiceBoard::goTime('2026-10-18 00:00:00+00'),
    '非 UTC 部署下按本地当前偏移渲染（与 Go 的 time.Local 一致）'
);
same(
    '0000-12-31T23:54:17+08:00',
    ServiceBoard::goTime('0001-01-01 00:00:00+08:05:43'),
    '零值哨兵按 +08:00 渲染，不套历史 LMT（+08:05:43）'
);
date_default_timezone_set('UTC');

same(3, JsonResponse::number(3.0), '整数值浮点转 int');
same(3.5, JsonResponse::number(3.5), '非整数保持浮点');
same(0, JsonResponse::number(null), 'null → 0');

// ---------------------------------------------------------------------------
// 结果
// ---------------------------------------------------------------------------

$total = $passed + count($failed);
if ($failed !== []) {
    fwrite(STDERR, "FAILED " . count($failed) . "/{$total}\n");
    foreach ($failed as $message) {
        fwrite(STDERR, '  - ' . $message . "\n");
    }
    exit(1);
}
echo "OK {$total} assertions\n";
