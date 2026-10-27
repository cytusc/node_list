<?php
declare(strict_types=1);

/**
 * 集成测试：连真实 PostgreSQL（node_explorer_live_* 视图）跑查询并断言契约。
 *
 * 前置：
 *   1. 一个可写的测试库，且已导入 schema 与夹具：
 *        PGURL="postgresql://postgres:test_password@127.0.0.1:5432/node_explorer_test" \
 *          bash tests/fixtures/load.sh
 *   2. 环境变量指向该库（与生产同名）：PGHOST / PGPORT / PGDATABASE / PGUSER / PGPASSWORD
 *
 * 用法：
 *   PGHOST=127.0.0.1 PGUSER=postgres PGPASSWORD=... PGDATABASE=node_explorer_test \
 *     php tests/integration.php
 *
 * 脚本会先把夹具里的观测时间戳推到「2 分钟前」——优选位判定要求证据在 45 分钟内，
 * 写死的时间戳放久了会让 preferred 排序失效，导致断言随「什么时候导入的」而飘。
 */

require __DIR__ . '/bootstrap.php';

use app\service\LiveResults;
use app\service\ResultsQueryParser;
use support\Db;

$passed = 0;
$failed = [];

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

// ---------------------------------------------------------------------------
// 0. 前置检查：库里有夹具数据，否则直接说明怎么准备
// ---------------------------------------------------------------------------

try {
    $count = (int)Db::selectOne('SELECT count(*) AS total FROM node_explorer_live_nodes')->total;
} catch (Throwable $e) {
    fwrite(STDERR, "无法连接测试库：{$e->getMessage()}\n");
    fwrite(STDERR, "请先执行 tests/fixtures/load.sh，并确认 PGHOST/PGDATABASE/PGUSER/PGPASSWORD。\n");
    exit(2);
}
if ($count === 0) {
    fwrite(STDERR, "视图里没有数据。请先执行：bash tests/fixtures/load.sh\n");
    exit(2);
}

// 把观测时间推到 2 分钟前（fixture 里 evidence 只有一条，路径固定）。
$stamp = "to_char(now()-interval '2 minutes','YYYY-MM-DD\"T\"HH24:MI:SSOF')";
Db::statement("UPDATE node_explorer_connectivity
  SET checked_at = now()-interval '2 minutes',
      payload = jsonb_set(payload,'{checkedAt}', to_jsonb({$stamp}))");
Db::statement("UPDATE node_explorer_exits
  SET checked_at = now()-interval '2 minutes',
      payload = jsonb_set(payload,'{checkedAt}', to_jsonb({$stamp}))");
Db::statement("UPDATE node_explorer_ip_attributes
  SET checked_at = now()-interval '2 minutes',
      payload = jsonb_set(
        jsonb_set(payload,'{checkedAt}', to_jsonb({$stamp})),
        '{classification,checkedAt}', to_jsonb({$stamp}))");

$live = new LiveResults();

/** 跑一次查询，返回响应体。 */
$run = static fn (array $params): array => $live->query(ResultsQueryParser::fromQuery($params));

// ---------------------------------------------------------------------------
// 1. 视图口径：available + 近期连通性记录 + 已验证出口决定展示
// ---------------------------------------------------------------------------

$all = $run([]);
same(5, $all['totalNodes'], 'live 视图包含 available=true 且近期测活、出口已验证的节点');
same(0, $all['page'], '不分页时 page=0');
same(0, $all['limit'], '不分页时 limit=0');
same(5, count($all['nodes']), '不分页时返回全部节点');
ok(!array_key_exists('checkedAt', $all), '结果不再包含批次检测时间');
ok(!array_key_exists('snapshotId', $all['nodes'][0]), '节点响应不包含快照标识');

$names = array_column($all['nodes'], 'name');
ok(!in_array('🇺🇸 美国 99', $names, true), '历史退役且 available=false 的节点不出现在结果里');
ok(!in_array('🇬🇧 英国 07', $names, true), 'available=false 的节点不出现在结果里');

Db::statement("UPDATE node_explorer_node_registry SET available=true WHERE identity='id-us-retired'");
try {
    $withRetired = $run([]);
    same(6, $withRetired['totalNodes'], '历史退役记录不能覆盖测活维护的 available=true');
    ok(in_array('🇺🇸 美国 99', array_column($withRetired['nodes'], 'name'), true), '近期测活与已验证出口齐全时，恢复可用后可正常展示');
} finally {
    Db::statement("UPDATE node_explorer_node_registry SET available=false WHERE identity='id-us-retired'");
}


// 缺少任一发布事实都要隐藏节点；调度标记保留，以便后续探针恢复。
foreach ([
    '负面测活结论' => "UPDATE node_explorer_connectivity SET payload=payload || '{\"reachable\":false}'::jsonb WHERE identity='id-us-ss'",
    '旧版测活记录' => "UPDATE node_explorer_connectivity SET payload=payload-'configDigest' WHERE identity='id-us-ss'",
    '配置已改变' => "UPDATE node_explorer_node_registry SET node=node || '{\"name\":\"changed\"}'::jsonb WHERE identity='id-us-ss'",
    '节点已替换' => "UPDATE node_explorer_node_registry SET incarnation='replacement' WHERE identity='id-us-ss'",
    '过期连通性记录' => "UPDATE node_explorer_connectivity SET checked_at=now()-interval '7 hours' WHERE identity='id-us-ss'",
    '缺少连通性记录' => "DELETE FROM node_explorer_connectivity WHERE identity='id-us-ss'",
    '出口未验证' => "UPDATE node_explorer_exits SET payload=jsonb_set(payload,'{verified}','false'::jsonb) WHERE identity='id-us-ss'",
    '出口缺少验证字段' => "UPDATE node_explorer_exits SET payload=payload-'verified' WHERE identity='id-us-ss'",
    '缺少出口记录' => "DELETE FROM node_explorer_exits WHERE identity='id-us-ss'",
] as $reason => $sql) {
    Db::beginTransaction();
    try {
        Db::statement($sql);
        $hidden = $run([]);
        same(4, $hidden['totalNodes'], $reason . '时不发布节点');
        ok(!in_array('id-us-ss', array_column($hidden['nodes'], 'identity'), true), $reason . '时响应不包含该节点');
        same(true, Db::table('node_explorer_node_registry')->where('identity', 'id-us-ss')->value('available'), $reason . '不改变调度标记');
    } finally {
        Db::rollBack();
    }
    same(5, $run([])['totalNodes'], $reason . '恢复后立即重新展示');
}

// ---------------------------------------------------------------------------
// 2. 分页
// ---------------------------------------------------------------------------

$page1 = $run(['page' => '1', 'limit' => '2']);
same(2, count($page1['nodes']), 'limit=2 返回 2 条');
same(5, $page1['total'], 'total 是过滤后的总数，不是当页条数');
same(1, $page1['page'], 'page 回显');
same(2, $page1['limit'], 'limit 回显');

$page3 = $run(['page' => '3', 'limit' => '2']);
same(3, $page3['page'], '第 3 页');
same(1, count($page3['nodes']), '最后一页只有 1 条');

$overflow = $run(['page' => '99', 'limit' => '2']);
same(3, $overflow['page'], '页码越界时夹到最后一页（Go 的 least/greatest）');

// ---------------------------------------------------------------------------
// 3. 排序
// ---------------------------------------------------------------------------

$preferred = $run(['page' => '1', 'limit' => '1', 'sort' => 'preferred']);
same('🇺🇸 美国 01', $preferred['nodes'][0]['name'] ?? null, '优选排序把住宅+新鲜证据的节点排第一');

$bySpeed = $run(['page' => '1', 'limit' => '1', 'sort' => 'speed', 'direction' => 'desc']);
same('🇯🇵 日本 10', $bySpeed['nodes'][0]['name'] ?? null, 'speed desc 第一名是最快节点');

$bySpeedAsc = $run(['page' => '1', 'limit' => '5', 'sort' => 'speed', 'direction' => 'asc']);
same('🇩🇪 德国 02', $bySpeedAsc['nodes'][0]['name'] ?? null, 'speed asc 第一条是速度 0 的节点');

$byCountry = $run(['page' => '1', 'limit' => '5', 'sort' => 'country', 'direction' => 'asc']);
same('🇩🇪 德国 02', $byCountry['nodes'][0]['name'] ?? null, 'country asc 按地区代码排序（DE 最前）');

// ---------------------------------------------------------------------------
// 4. 筛选
// ---------------------------------------------------------------------------

same(1, $run(['page' => '1', 'limit' => '50', 'country' => 'US'])['total'], 'country=US');
same(2, $run(['page' => '1', 'limit' => '50', 'type' => 'ss'])['total'], 'type=ss');
same(1, $run(['page' => '1', 'limit' => '50', 'features' => 'reality'])['total'], 'features=reality');
same(4, $run(['page' => '1', 'limit' => '50', 'features' => 'tls'])['total'], 'features=tls');
same(1, $run(['page' => '1', 'limit' => '50', 'features' => 'tls,reality'])['total'], '多个 features 是 AND');

// 中文地区名搜索：走「名称 → 地区代码」展开，不依赖 DB 里的中文
same(1, $run(['page' => '1', 'limit' => '50', 'q' => '美国'])['total'], '搜索「美国」命中 US 节点');
same(1, $run(['page' => '1', 'limit' => '50', 'q' => '香港'])['total'], '搜索「香港」命中名称匹配');
same(3, $run(['page' => '1', 'limit' => '50', 'q' => 'example.com'])['total'], '搜索域名命中 sni（夹具里只有前 3 个节点配了 sni）');

// media 是「全部包含」语义
same(1, $run(['page' => '1', 'limit' => '50', 'media' => 'Netflix,YouTube'])['total'], 'media 多选是包含判定');
same(3, $run(['page' => '1', 'limit' => '50', 'media' => 'Netflix'])['total'], 'media 单选');

same(1, $run(['page' => '1', 'limit' => '50', 'ipSource' => 'ippure'])['total'], 'ipSource=ippure');
same(1, $run(['page' => '1', 'limit' => '50', 'ipAttributes' => 'residential'])['total'], 'ipAttributes=residential');
same(1, $run(['page' => '1', 'limit' => '50', 'minSpeed' => '8000'])['total'], 'minSpeed 阈值过滤');
same(4, $run(['page' => '1', 'limit' => '50', 'minSpeed' => '1'])['total'], 'minSpeed=1 仍会滤掉 speed=0 的节点（德国 02）');

// ---------------------------------------------------------------------------
// 5. facets / summary
// ---------------------------------------------------------------------------

$facets = $all['facets'];
same(
    ['country', 'type', 'network', 'ipSource', 'ipAttributes', 'features', 'media'],
    array_keys($facets),
    'facet 分桶齐全'
);
$countryValues = array_column($facets['country'], 'value');
ok(in_array('unknown', $countryValues, true), '空 country 归入 unknown 桶');
$unknownLabel = $facets['country'][array_search('unknown', $countryValues, true)]['label'] ?? null;
same('未知地区', $unknownLabel, 'unknown 桶的标签是中文');
same(['tls', 'reality', 'udp'], array_column($facets['features'], 'value'), 'features 固定三项且顺序稳定');
same(4, $facets['features'][0]['count'], 'tls 计数');
same(1, $facets['features'][1]['count'], 'reality 计数');
same(3, $facets['media'][0]['count'], 'Netflix 计数');
// facets 排序：count DESC，同 count 按 ICU 数字序
$counts = array_column($facets['media'], 'count');
$sorted = $counts;
rsort($sorted);
same($sorted, $counts, 'facets 按 count 降序');

same(4, $all['summary']['countries'], 'summary.countries 不计空地区');
same(10485760, $all['summary']['maxSpeed'], 'summary.maxSpeed');
same(6291456, $all['summary']['median'], 'summary.median 是偶数个样本的中位（含 0 速度被排除）');
same(true, $all['speedTest'], 'speedTest 标志');
same(true, $all['mediaCheck'], 'mediaCheck 标志');

// ---------------------------------------------------------------------------
// 6. 节点载荷：凭证剥离 + 富集信息形状
// ---------------------------------------------------------------------------

$us = null;
foreach ($all['nodes'] as $node) {
    if (($node['name'] ?? '') === '🇺🇸 美国 01') {
        $us = $node;
    }
}
ok($us !== null, '找到美国节点');
ok(!array_key_exists('rawLink', $us ?? []), '节点的 rawLink 不出现在响应里');
ok(!array_key_exists('rawConfig', $us ?? []), '节点的 rawConfig 不出现在响应里');
ok(!array_key_exists('generatedLink', $us ?? []), '生成链接不出现在列表响应里');
same(true, $us['hasOriginalLink'] ?? null, '原始链接存在时可复制');
foreach ($all['nodes'] as $node) {
    if ($node['identity'] !== 'id-us-ss') same(false, $node['hasOriginalLink'] ?? null, '无原始链接的节点不可复制');
}
Db::statement("UPDATE node_explorer_node_registry SET node=node || jsonb_build_object('generatedLink','http://generated:test@198.51.100.20:8443') WHERE identity='id-jp-vmess'");
$generatedPage = $run(['page' => '1', 'limit' => '50']);
foreach ($generatedPage['nodes'] as $item) {
    if ($item['identity'] === 'id-jp-vmess') {
        same(true, $item['hasOriginalLink'] ?? null, '已保存生成链接的节点可复制');
        ok(!array_key_exists('generatedLink', $item), '生成链接凭据不通过列表泄露');
    }
}
Db::statement("UPDATE node_explorer_node_registry SET node=node-'generatedLink' WHERE identity='id-jp-vmess'");

same('443', $us['port'] ?? null, 'port 统一为字符串');
same(['status' => 'active'], $us['lifecycle'] ?? null, 'lifecycle 固定 active');
same('residential', $us['ippure']['classification']['state'] ?? null, 'ippure.classification 透出住宅判定');
same(false, $us['ippure']['classification']['evidence'][0]['hosting'] ?? null, 'evidence.hosting=false 保留');
same('2026-10-18T00:00:00Z', $us['checkedAt'] ?? null, '节点 checkedAt');

// IPv6 出口：ip_status 被视图判为 unsupported，ippure 里要有明确状态
$sg = null;
foreach ($all['nodes'] as $node) {
    if (($node['name'] ?? '') === '🇸🇬 新加坡 01') {
        $sg = $node;
    }
}
same('unsupported', $sg['ippure']['status'] ?? null, '仅 IPv6 的出口标记为 unsupported');
same(true, $sg['connectivity']['reachable'] ?? null, '已发布节点保留正面测活结论');

// 出口已验证但属性尚未查询的节点仍可展示，返回待查询状态。
$hk = null;
foreach ($all['nodes'] as $node) {
    if (($node['name'] ?? '') === '🇭🇰 香港 03') {
        $hk = $node;
    }
}
same('error', $hk['ippure']['status'] ?? null, '出口已验证但缺少属性时返回待查询状态');
same('ippure-query', $hk['ippure']['failureStage'] ?? null, '缺少属性不等于缺少已验证出口');
same('203.0.113.50', $hk['ippure']['ip'] ?? null, '待查询属性保留已验证出口 IP');

// 标签统计和筛选必须与实际返回的详情一致，不能只读取库里的旧 attributes。
foreach ($all['facets']['ipAttributes'] as $facet) {
    $matching = array_values(array_filter($all['nodes'], static fn (array $node): bool =>
        in_array($facet['value'], $node['ippure']['attributes'] ?? [], true)));
    same(count($matching), $facet['count'], 'facet 计数与详情标签一致：' . $facet['value']);
    $filtered = $run(['page' => '1', 'limit' => '50', 'ipAttributes' => $facet['value']]);
    same(count($matching), $filtered['total'], '筛选数量与详情标签一致：' . $facet['value']);
}
same(1, $run(['page' => '1', 'ipAttributes' => '住宅IP'])['total'], '详情派生的住宅IP 标签也可筛选');

// 模拟历史分类标签未更新、风险查询失败；类型证据仍支持住宅。
Db::beginTransaction();
try {
    Db::table('node_explorer_ip_attributes')->where('ip', '203.0.113.10')->update([
        'payload' => Db::raw("jsonb_set(jsonb_set(jsonb_set(payload,
            '{attributes}', '[\"机房IP\"]'::jsonb),
            '{classification,state}', '\"datacenter\"'::jsonb), '{status}', '\"error\"'::jsonb)"),
    ]);
    $reclassified = $run(['page' => '1', 'ipAttributes' => '住宅IP']);
    same(1, $reclassified['total'], '旧标签与风险失败不能使住宅节点从筛选中消失');
    same('🇺🇸 美国 01', $reclassified['nodes'][0]['name'] ?? null, '返回遗漏的住宅节点');
    same(['住宅IP'], $reclassified['nodes'][0]['ippure']['attributes'] ?? null, '筛选后的详情标签一致');
    same('🇺🇸 美国 01', $run(['page' => '1', 'limit' => '1'])['nodes'][0]['name'] ?? null, '旧分类重算后仍在首页优选');
    $stored = json_decode(Db::table('node_explorer_ip_attributes')->where('ip', '203.0.113.10')->value('payload'));
    same('datacenter', $stored->classification->state, '查询不回写数据库分类或时间');
} finally {
    Db::rollBack();
}

// ---------------------------------------------------------------------------
// 7. 每次请求都读取当前可用状态，不缓存结果，也不附带采集器状态。
// ---------------------------------------------------------------------------
$before = $run(['page' => '1', 'limit' => '50']);
Db::statement("UPDATE node_explorer_node_registry SET available=false WHERE identity='id-us-ss'");
try {
    $after = $run(['page' => '1', 'limit' => '50']);
    same($before['totalNodes'] - 1, $after['totalNodes'], '可用状态改变后下次请求立即生效');
    ok(!in_array('id-us-ss', array_column($after['nodes'], 'identity'), true), '不可用节点不出现在响应中');
    ok(!array_key_exists('sourceWarning', $after), '不附带来源告警');
    ok(!array_key_exists('ippureCollector', $after), '不附带采集器状态');
} finally {
    Db::statement("UPDATE node_explorer_node_registry SET available=true WHERE identity='id-us-ss'");
}
same($before['totalNodes'], $run([])['totalNodes'], '恢复可用后下次请求立即展示');

// ---------------------------------------------------------------------------
// 结果
// ---------------------------------------------------------------------------

require __DIR__ . '/monitor-integration.php';

$total = $passed + count($failed);
if ($failed !== []) {
    fwrite(STDERR, "FAILED " . count($failed) . "/{$total}\n");
    foreach ($failed as $message) {
        fwrite(STDERR, '  - ' . $message . "\n");
    }
    exit(1);
}
echo "OK {$total} assertions\n";
