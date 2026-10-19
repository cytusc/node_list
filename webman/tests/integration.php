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
 * 脚本会先把夹具里的观测时间戳推到「2 分钟前」——优选位判定要求证据在 10 分钟内，
 * 写死的时间戳放久了会让 preferred 排序失效，导致断言随「什么时候导入的」而飘。
 */

require __DIR__ . '/bootstrap.php';

use app\service\Heartbeat;
use app\service\LiveResults;
use app\service\ReadCache;
use app\service\ResultsQueryParser;
use app\service\ServiceBoard;
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
// 1. 视图口径：退役与下线节点必须被排除
// ---------------------------------------------------------------------------

$all = $run([]);
same(5, $all['totalNodes'], 'live 视图只含可用且未退役的节点');
same(0, $all['page'], '不分页时 page=0');
same(0, $all['limit'], '不分页时 limit=0');
same(5, count($all['nodes']), '不分页时返回全部节点');
same('2026-10-18T00:00:00Z', $all['checkedAt'], '批次时间透传');

$names = array_column($all['nodes'], 'name');
ok(!in_array('🇺🇸 美国 99', $names, true), '退役节点不出现在结果里');
ok(!in_array('🇬🇧 英国 07', $names, true), 'available=false 的节点不出现在结果里');

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
ok(isset($sg['connectivity']['error']), '连通性失败原因透出');

// 没有富集数据的节点不应凭空出现 ippure
$hk = null;
foreach ($all['nodes'] as $node) {
    if (($node['name'] ?? '') === '🇭🇰 香港 03') {
        $hk = $node;
    }
}
ok(!array_key_exists('ippure', $hk ?? []), '无出口探针数据的节点没有 ippure 字段');

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
// 7. 服务看板与附属字段
//
// 心跳表由测试自己掌管：本机可能同时跑着别的进程（比如并排验证 Go 版时，
// 它每 10s 写一次 api 心跳），不清空的话断言会随「谁在跑」而飘。
// 每改一次数据都要 forget 缓存，否则读到的是 10s 窗口里的旧值。
// ---------------------------------------------------------------------------

$board = new ServiceBoard();
// 心跳是进程内节流的：不清状态的话，同一进程里先跑过的调用会把本次写入
// 节流掉，断言就随执行顺序飘了。
Heartbeat::reset();
$cache = new ReadCache();
foreach (['node-list:services', 'node-list:collector', 'node-list:source-warning'] as $key) {
    $cache->forget($key);
}
Db::statement('DELETE FROM node_explorer_service_status');

$services = $board->services()['services'];
same(9, count($services), '看板固定 9 个服务');
same('api', $services[0]['service'], '按服务名排序');
// 字段集必须与 Go 的 service.Status 完全一致：多一个键就是契约漂移
// （内部行里的 updatedAtRfc3339 只供采集器状态用，不能透出去）。
// 在 source-ingest 行上断言：它的 error 非空，11 个键才会全部出现；
// api 行健康时 error 按 Go 的 omitempty 消失，那是下面单独断言的行为。
$ingest = array_column($services, null, 'service')['source-ingest'];
same(
    ['service', 'updatedAt', 'healthy', 'error', 'queued', 'scheduled', 'running',
     'oldestWaitSeconds', 'restored', 'checked', 'skipped',
     'activeAssessments', 'checkedLastHour', 'drainEtaSeconds'],
    array_keys($ingest),
    '服务状态字段与 Go 的 Status 结构一致'
);
// api 行是 webman 自己刚写的心跳（Heartbeat 在 statuses() 入口惰性写入）：
// 删掉 Go 的 node-explorer 之后，这一行必须仍然健康，否则就是心跳没接上。
same(true, $services[0]['healthy'], 'webman 自己写的 api 心跳算健康');
ok(!array_key_exists('error', $services[0]), '心跳 error 为空时不输出该字段（Go 的 omitempty）');

// 「无心跳」的文案与零值时间同样在 source-ingest 行上断言：webman 不写这个
// 服务，不受节流时序影响；而 api 行只要被读过就一定有心跳。
same('服务尚未启动', $ingest['error'], '无心跳时的文案');
// 零值时间的字面量取决于部署时区（PG 的 0001-01-01 哨兵 + Go 的「本地当前偏移」
// 渲染），所以只断言它是那个哨兵，不写死 Z 还是 +08:00。
$isZeroTime = static fn (string $v): bool =>
    str_starts_with($v, '0000-12-31') || str_starts_with($v, '0001-01-01');
ok($isZeroTime($ingest['updatedAt']), '无心跳时 updatedAt 是零值时间哨兵');
same(false, $ingest['healthy'], '无心跳时 healthy=false');

// 两类恢复任务的统计独立，出口失败和分类未确认也计入住宅核验结果。
Db::statement("INSERT INTO node_explorer_work(kind,key,state,due_at,payload) VALUES
 ('normal-recovery','board-low-scheduled','queued',now()+interval '6 hours','{}'),
 ('normal-recovery','board-low-completed','done',now(),'{\"recoveryResult\":\"eligible\"}'),
 ('recovery','board-high-exit-failed','done',now(),'{\"recoveryResult\":\"exit-probe-failed\"}'),
 ('recovery','board-high-unconfirmed','done',now(),'{\"recoveryResult\":\"classification-unconfirmed\"}')");
$split = array_column($board->statuses(['normal-recovery', 'residential-recovery']), null, 'service');
same(1, $split['normal-recovery']['scheduled'], '非住宅恢复独立统计定时等待');
same(1, $split['normal-recovery']['checked'], '非住宅恢复独立统计完成结果');
same(2, $split['residential-recovery']['checked'], '住宅核验包含出口失败和分类未确认');
// A finished assessment remains counted after requeue, polling does not count.
Db::statement("INSERT INTO node_explorer_work(kind,key,state,due_at,attempts,payload) VALUES
 ('normal-recovery','board-low-requeued','queued',now()+interval '6 hours',99,
 jsonb_build_object('recoveryResult','probe-failed','recoveryAttempts',3,'checkedAt',now(),
 'recoveryHistory',jsonb_build_array(
 jsonb_build_object('result','probe-failed','checkedAt',now()-interval '20 minutes'),
 jsonb_build_object('result','probe-failed','checkedAt',now()-interval '10 minutes'),
 jsonb_build_object('result','probe-failed','checkedAt',now())))),
 ('normal-recovery','board-low-active','queued',now()-interval '10 seconds',20,
 jsonb_build_object('startedAt',now()))");
$metrics = $board->statuses(['normal-recovery'])[0];
same(4, $metrics['checked'], '累计核验包含已重新排期的三轮结果');
same(3, $metrics['checkedLastHour'], '滚动吞吐按历史次数计算，忽略轮询次数');
same(1, $metrics['activeAssessments'], '等待依赖的会话仍计入核验中');
same(1200, $metrics['drainEtaSeconds'], '预计排空时间只计算到期任务');
Db::statement("DELETE FROM node_explorer_work WHERE key LIKE 'board-%'");

// 心跳过期 → 明确文案，且不再健康。先灌一行再改旧：source-ingest 没有
// 写入方，直接 UPDATE 会更新 0 行，读到的仍是「尚未启动」。
Db::statement(
    "INSERT INTO node_explorer_service_status(service,updated_at,payload)
     VALUES('source-ingest', now(), '{\"error\":\"\",\"running\":true}'::jsonb)
     ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload"
);
Db::statement("UPDATE node_explorer_service_status SET updated_at=now()-interval '5 minutes' WHERE service='source-ingest'");
$cache->forget('node-list:services');
$stale = array_column($board->services()['services'], null, 'service')['source-ingest'];
same(false, $stale['healthy'], '超过 90 秒的心跳不算健康');
same('服务心跳已过期', $stale['error'], '心跳过期的文案');

// 采集器状态读的是 ip-service：本夹具没有它的心跳。
$collector = $board->collectorStatus();
same(true, $collector['enabled'], 'collector.enabled 恒为 true');
ok($isZeroTime($collector['lastRunAt']), 'collector.lastRunAt 用秒精度零值时间哨兵');
same('服务尚未启动', $collector['error'], '采集器无心跳时透出错误文案');
same(null, $board->sourceWarning(), '无 sourceWarning 时为 null');

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
