<?php
declare(strict_types=1);

namespace app\service;

use app\service\enum\Direction;
use app\service\enum\FacetKind;
use app\service\enum\Feature;
use app\service\enum\SortKey;
use Closure;
use DateTimeImmutable;
use DateTimeInterface;
use stdClass;
use support\Db;
use support\Log;
use Throwable;

/**
 * 每次请求读取数据库实时视图，只展示 available=true 的节点。
 * 先读取筛选所需的字段，再按当页 identity 获取展示数据，避免逐行重算视图。
 * 请求内使用一致的数据库读事务，保证统计、分页和节点数据对应同一次读取。
 */
final class LiveResults
{
    /**
     * 执行一次结果查询。
     *
     * @param ResultsQuery|null $q null 表示「不分页、不过滤，整份返回」
     * @return array<string, mixed> 实时节点列表、统计和筛选项
     * @throws ApiError 数据库读失败 → 503 READ_FAILED
     */
    public function query(?ResultsQuery $q): array
    {
        try {
            return Db::connection()->transaction(function () use ($q): array {
                if (Db::connection()->transactionLevel() === 1) {
                    Db::statement('SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY');
                }
                $allRows = $this->normalizeFacts($this->factsQuery()->get()->all());
                $now = new DateTimeImmutable();
                [$pageRows, $total, $page, $limit] = $this->resolvePage($allRows, $q, $now);
                $payloads = $this->fetchPayloadsById(array_column($pageRows, 'identity'));
                return $this->buildBody($allRows, $pageRows, $payloads, $total, $page, $limit, $now);
            });
        } catch (Throwable $e) {
            Log::error('live results query failed: ' . $e->getMessage());
            throw ApiError::readFailed();
        }
    }

    /** facts 查询基于的视图（供测试断言「不含 JOIN」）。 */
    public static function factsTable(): string
    {
        return 'node_explorer_live_facts';
    }

    /** payload 查询基于的视图（供测试断言「不含 JOIN」）。 */
    public static function payloadsTable(): string
    {
        return 'node_explorer_live_nodes';
    }

    /**
     * facts 查询构造器：用 webman（Illuminate）查询构造器取派生列。
     *
     * 单张视图、扁平 SELECT，不含 JOIN / CTE / 分页。jsonb 拆解用 Db::raw 表达
     * （PG 特有，构造器本身无法表达），其余列走构造器的列名。数据库根据普通视图规划底层表查询。
     */
    private function factsQuery()
    {
        return Db::table(self::factsTable())->select([
            'identity', 'country', 'ip_status',
            Db::raw("coalesce(nullif(btrim(node->>'rawLink'),''),btrim(node->>'generatedLink'),'')<>'' AS has_original_link"),
            Db::raw("coalesce(nullif(country,''),'unknown') AS region"),
            Db::raw("node->>'name' AS name"),
            Db::raw("node->>'baseName' AS base_name"),
            Db::raw("node->>'server' AS server"),
            Db::raw("node->>'sni' AS sni"),
            Db::raw("node->>'type' AS node_type"),
            Db::raw("coalesce(nullif(node->>'type',''),'unknown') AS protocol"),
            Db::raw("coalesce(nullif(node->>'network',''),'unknown') AS network"),
            Db::raw("coalesce((node->>'speed')::double precision,0) AS speed"),
            Db::raw("coalesce((node->>'tls')::boolean,false) AS tls"),
            Db::raw("coalesce((node->>'reality')::boolean,false) AS reality"),
            Db::raw("coalesce((node->>'udp')::boolean,false) AS udp"),
            Db::raw("(node->>'speedTest')::boolean AS speed_test"),
            Db::raw("(node->>'mediaCheck')::boolean AS media_check"),
            Db::raw("attributes->>'ip' AS attributes_ip"),
            Db::raw("attributes->>'source' AS ip_source"),
            Db::raw("attributes->>'asnOrganization' AS asn_organization"),
            Db::raw("(attributes->>'score')::double precision AS ip_score"),
            Db::raw("(attributes->>'humanPercent')::double precision AS human_percent"),
            Db::raw("attributes->'classification' AS classification"),
            Db::raw("to_jsonb(ARRAY(SELECT jsonb_array_elements_text(coalesce(nullif(attributes->'attributes','null'::jsonb),'[]'::jsonb)))) AS ip_attributes"),
            Db::raw("coalesce((connectivity->>'reachable')::boolean,false) AS reachable"),
            Db::raw("connectivity->>'error' AS connectivity_error"),
            Db::raw("connectivity->>'checkedAt' AS connectivity_at"),
            Db::raw("exit_payload->>'ip' AS reported_exit_ip"),
            Db::raw("exit_payload->>'error' AS exit_error"),
            Db::raw("exit_payload->>'checkedAt' AS exit_at"),
            Db::raw("to_jsonb(ARRAY(SELECT DISTINCT coalesce(nullif(m->>'platform',''),nullif(m->>'Platform',''),nullif(m->>'tag',''),m->>'Tag') FROM jsonb_array_elements(coalesce(nullif(node->'media','null'::jsonb),'[]'::jsonb)) m WHERE coalesce(nullif(m->>'tag',''),nullif(m->>'Tag',''),nullif(m->>'platform',''),m->>'Platform','')<>'')) AS media"),
        ]);
    }

    /**
     * 把视图返回的行规范化为统一的派生列数组。
     *
     * PDO 把 jsonb 列以字符串形式返回（SQL NULL 则是 PHP null），这里统一解码：
     * classification 解码成对象，ip_attributes / media 解码成字符串数组，
     * 其它标量按类型修正（bool 形态因 PDO 版本而异，统一走 toBool）。
     *
     * @param list<stdClass> $raw
     * @return list<array<string, mixed>>
     */
    public function normalizeFacts(array $raw): array
    {
        $rows = [];
        foreach ($raw as $f) {
            $classification = $this->decodeJson($f->classification ?? null);
            $ipAttributes = $this->decodeJson($f->ip_attributes ?? null);
            $media = $this->decodeJson($f->media ?? null);
            $ipResult = IpClassification::normalize((object)[
                'ip' => $f->attributes_ip ?? '',
                'classification' => is_object($classification) ? $classification : null,
                'attributes' => is_array($ipAttributes) ? $ipAttributes : [],
            ]);
            $rows[] = [
                'identity' => $f->identity,
                'has_original_link' => self::toBool($f->has_original_link ?? false),
                'country' => $f->country ?? '',
                'region' => $f->region ?? ($f->country ?? ''),
                'name' => $f->name ?? '',
                'base_name' => $f->base_name ?? '',
                'server' => $f->server ?? '',
                'sni' => $f->sni ?? '',
                'node_type' => $f->node_type ?? '',
                'protocol' => $f->protocol ?? 'unknown',
                'network' => $f->network ?? 'unknown',
                'speed' => (float)($f->speed ?? 0),
                'tls' => self::toBool($f->tls ?? false),
                'reality' => self::toBool($f->reality ?? false),
                'udp' => self::toBool($f->udp ?? false),
                'speed_test' => self::toBool($f->speed_test ?? false),
                'media_check' => self::toBool($f->media_check ?? false),
                'attributes_ip' => $f->attributes_ip ?? '',
                'ip_status' => $f->ip_status ?? '',
                'ip_source' => $f->ip_source ?? '',
                'asn_organization' => $f->asn_organization ?? '',
                'ip_score' => ($f->ip_score === null || $f->ip_score === '') ? null : (float)$f->ip_score,
                'human_percent' => ($f->human_percent === null || $f->human_percent === '') ? null : (float)$f->human_percent,
                'classification' => $ipResult->classification,
                'ip_attributes' => $ipResult->attributes,
                'reachable' => self::toBool($f->reachable ?? false),
                'connectivity_error' => $f->connectivity_error ?? '',
                'connectivity_at' => $f->connectivity_at ?? '',
                'reported_exit_ip' => $f->reported_exit_ip ?? '',
                'exit_error' => $f->exit_error ?? '',
                'exit_at' => $f->exit_at ?? '',
                'media' => is_array($media) ? $media : [],
            ];
        }
        return $rows;
    }

    /**
     * 过滤 + 排序 + 分页，全部在 PHP 内存里完成。
     *
     * 与 Go 的 liveRows/liveSort 语义一致：
     *   - summary / facets 永远基于「全量节点」（见 buildBody），不受筛选影响；
     *   - total / 当页节点才受 $q 的筛选影响。
     *
     * @param list<array<string, mixed>> $allRows
     * @return array{0:list<array<string,mixed>>, 1:int, 2:int, 3:int} [pageRows, total, page, limit]
     */
    public function resolvePage(array $allRows, ?ResultsQuery $q, DateTimeImmutable $now): array
    {
        if ($q === null) {
            $ordered = $this->sortRows($allRows, null, $this->anyFlag($allRows, 'speed_test'), $now);
            return [$ordered, count($allRows), 0, 0];
        }

        $speedTest = $this->anyFlag($allRows, 'speed_test');
        $mediaCheck = $this->anyFlag($allRows, 'media_check');

        $filtered = $this->filterRows($allRows, $q, $speedTest, $mediaCheck);
        $ordered = $this->sortRows($filtered, $q, $speedTest, $now);

        $total = count($ordered);
        $limit = $q->limit;
        // 页码越界时夹到最后一页（Go 的 least/greatest 语义）。total=0 时回到第 1 页。
        $pages = $total === 0 ? 1 : (int)ceil($total / $limit);
        $page = min($q->page, max(1, $pages));
        $offset = ($page - 1) * $limit;

        return [array_slice($ordered, $offset, $limit), $total, $page, $limit];
    }

    /**
     * 组装响应体。summary / facets 用全量节点，nodes 用当页节点。
     *
     * @param list<array<string, mixed>> $allRows
     * @param list<array<string, mixed>> $pageRows
     * @param array<string, stdClass> $payloads identity => 解码后的节点 payload
     * @return array<string, mixed>
     */
    public function buildBody(array $allRows, array $pageRows, array $payloads, int $total, int $page, int $limit, DateTimeImmutable $now): array
    {
        $totalNodes = count($allRows);
        $countrySet = [];
        $maxSpeed = 0.0;
        $speeds = [];
        $speedTest = false;
        $mediaCheck = false;

        foreach ($allRows as $r) {
            $c = $r['country'] ?? '';
            if ($c !== '') {
                $countrySet[$c] = true;
            }
            if ($r['speed'] > 0) {
                $maxSpeed = max($maxSpeed, $r['speed']);
                $speeds[] = $r['speed'];
            }
            if ($r['speed_test']) {
                $speedTest = true;
            }
            if ($r['media_check']) {
                $mediaCheck = true;
            }
        }

        $facetEntries = $this->computeFacets($allRows);

        $pagePayloads = [];
        foreach ($pageRows as $r) {
            $pagePayloads[] = $payloads[$r['identity']] ?? new stdClass();
        }
        $nodes = NodePresenter::nodes($pagePayloads);
        foreach ($nodes as $index => &$node) {
            $node['identity'] = $pageRows[$index]['identity'];
            $node['hasOriginalLink'] = $pageRows[$index]['has_original_link'] ?? false;
        }
        unset($node);

        $body = [];
        $body['speedTest'] = $speedTest;
        $body['mediaCheck'] = $mediaCheck;
        $body['totalNodes'] = $totalNodes;
        $body['summary'] = [
            'countries' => count($countrySet),
            'maxSpeed' => JsonResponse::number($maxSpeed),
            'median' => JsonResponse::number($this->median($speeds)),
        ];
        $body['facets'] = self::facets($facetEntries);
        $body['nodes'] = $nodes;
        $body['total'] = $total;
        $body['page'] = $page;
        $body['limit'] = $limit;

        return $body;
    }

    /**
     * 筛选（与 Go 的 liveFilter / 旧 filters() 等价）。
     *
     * 注意 minSpeed / media 两处依赖「全量是否做过测速 / 媒体探测」的全局标志，
     * 语义来自 SQL 里的 (NOT (SELECT speed_test FROM metadata) ...) / media_check。
     *
     * @param list<array<string, mixed>> $rows
     * @return list<array<string, mixed>>
     */
    private function filterRows(array $rows, ResultsQuery $q, bool $speedTest, bool $mediaCheck): array
    {
        return array_values(array_filter($rows, fn (array $r): bool => $this->matchRow($r, $q, $speedTest, $mediaCheck)));
    }

    private function matchRow(array $r, ResultsQuery $q, bool $speedTest, bool $mediaCheck): bool
    {
        if ($q->countries !== [] && !in_array($r['region'], $q->countries, true)) {
            return false;
        }
        if ($q->types !== [] && !in_array($r['protocol'], $q->types, true)) {
            return false;
        }
        if ($q->networks !== [] && !in_array($r['network'], $q->networks, true)) {
            return false;
        }
        foreach ($q->features as $feature) {
            if (!($r[$feature->column()] ?? false)) {
                return false;
            }
        }
        if ($q->search !== '') {
            $needle = mb_strtolower($q->search, 'UTF-8');
            $hit = false;
            foreach ([$r['name'], $r['base_name'], $r['server'], $r['sni'], $r['country']] as $field) {
                if (str_contains(mb_strtolower((string)$field, 'UTF-8'), $needle)) {
                    $hit = true;
                    break;
                }
            }
            if (!$hit) {
                // 中文地区名 → 地区代码集合（与 Go 的搜索前置展开一致）。
                $codes = Country::codesMatching($q->search);
                if (!in_array($r['region'], $codes, true)) {
                    return false;
                }
            }
        }
        if ($q->ipSources !== []) {
            if ($r['ip_status'] !== 'success') {
                return false;
            }
            if (!in_array($r['ip_source'], $q->ipSources, true)) {
                return false;
            }
        }
        if ($q->ipAttributes !== []) {
            if ($r['ip_status'] !== 'success' && !is_object($r['classification'])) {
                return false;
            }
            if (array_intersect($r['ip_attributes'], $q->ipAttributes) === []) {
                return false;
            }
        }
        if ($q->minSpeed > 0 && $speedTest && $r['speed'] < $q->minSpeed * 1024) {
            return false;
        }
        if ($q->media !== []) {
            // 全量未做媒体探测时不过滤；否则要求全部选中项都出现在节点 media 里。
            if ($mediaCheck && array_diff($q->media, $r['media']) !== []) {
                return false;
            }
        }
        return true;
    }

    /**
     * 排序键生成（与旧 order() 等价）。
     *
     * 分支穷尽且互斥：优选位 / 速度特例 / 枚举列 / 缺省。每个排序值一次性预计算
     * （见 sortRows 的装饰步骤），避免比较器里反复解析时间戳。NULLS LAST 在
     * cmpValues 里统一处理，与 SQL 的 NULLS LAST 一致。
     *
     * @return list<array{0:Closure, 1:string, 2:string}> [提取器, 方向, 比较方式]
     */
    private function sortKeys(?ResultsQuery $q, bool $speedTest, DateTimeImmutable $now): array
    {
        $keys = [];

        if ($q === null || $q->preferred || $q->sort === null) {
            $keys[] = [fn (array $r): mixed => $this->isPreferred($r, $now) ? 1 : 0, 'DESC', 'numeric'];
            if ($speedTest) {
                $keys[] = [fn (array $r): mixed => $r['speed'], 'DESC', 'numeric'];
            }
            if (!$speedTest) {
                $keys[] = [$this->nameExtractor(), 'ASC', 'natural'];
            }
        } elseif ($q->sort === SortKey::Speed) {
            $dir = $q->direction->sql();
            if ($speedTest) {
                $keys[] = [fn (array $r): mixed => $r['speed'], $dir, 'numeric'];
            }
            if (!$speedTest) {
                $keys[] = [$this->nameExtractor(), 'ASC', 'natural'];
            }
        } else {
            $k = $q->sort;
            $keys[] = [$this->keyExtractor($k), $q->direction->sql(), $k->isNumeric() ? 'numeric' : 'natural'];
        }

        // 用节点标识保证分页排序稳定。
        $keys[] = [fn (array $r): mixed => $r['identity'], 'ASC', 'natural'];

        return $keys;
    }

    /**
     * 内存排序。
     *
     * 先按 sortKeys 把每行需要用到的排序值预计算一遍（isPreferred 等重活只跑一次），
     * 再用预计算值做稳定的多键比较，避免 O(n log n) 比较里反复解析时间戳。
     *
     * @param list<array<string, mixed>> $rows
     * @return list<array<string, mixed>>
     */
    private function sortRows(array $rows, ?ResultsQuery $q, bool $speedTest, DateTimeImmutable $now): array
    {
        $keys = $this->sortKeys($q, $speedTest, $now);

        $decorated = array_map(function (array $r) use ($keys): array {
            $vals = [];
            foreach ($keys as [$extract]) {
                $vals[] = $extract($r);
            }
            return [$r, $vals];
        }, $rows);

        usort($decorated, function (array $A, array $B) use ($keys): int {
            foreach ($keys as $i => [$extract, $dir, $natural]) {
                $c = $this->cmpValues($A[1][$i], $B[1][$i], $dir, $natural);
                if ($c !== 0) {
                    return $c;
                }
            }
            return 0;
        });

        return array_column($decorated, 0);
    }

    /** 名称排序键：剥离国旗 emoji（与 SQL 的 regexp_replace 一致）。 */
    private function nameExtractor(): Closure
    {
        return fn (array $r): mixed => $this->stripEmoji($r['base_name'] !== '' ? $r['base_name'] : $r['name']);
    }

    /** 各排序键的取值提取器（同 Go 的 projection.SortColumn）。 */
    private function keyExtractor(SortKey $k): Closure
    {
        return match ($k) {
            SortKey::Country => fn (array $r): mixed => $r['country'] === '' ? null : $r['country'],
            SortKey::Name => $this->nameExtractor(),
            SortKey::Type => fn (array $r): mixed => $r['node_type'] === '' ? null : $r['node_type'],
            SortKey::AsnOrganization => fn (array $r): mixed => $r['ip_status'] === 'success'
                ? ($r['asn_organization'] === '' ? null : $r['asn_organization'])
                : null,
            SortKey::IPSource => fn (array $r): mixed => $r['ip_status'] === 'success'
                ? ($r['ip_source'] === '' ? null : $r['ip_source'])
                : null,
            SortKey::IPAttributes => fn (array $r): mixed => ($r['ip_status'] === 'success' || is_object($r['classification']))
                ? (implode(' · ', $r['ip_attributes']) === '' ? null : implode(' · ', $r['ip_attributes']))
                : null,
            SortKey::IPPure => fn (array $r): mixed => $r['ip_status'] === 'success' ? ($r['ip_score'] ?? 0) : null,
            SortKey::TrafficRatio => fn (array $r): mixed => $r['ip_status'] === 'success' ? $r['human_percent'] : null,
            SortKey::Speed => fn (array $r): mixed => $r['speed'],
        };
    }

    /**
     * 多键比较，统一处理 NULLS LAST（与 SQL 的 NULLS LAST 一致：无论升序降序，
     * 空值都排到最后）。
     *
     * @param mixed  $a
     * @param mixed  $b
     */
    private function cmpValues($a, $b, string $dir, string $natural): int
    {
        $aNull = $a === null;
        $bNull = $b === null;
        if ($aNull && $bNull) {
            return 0;
        }
        if ($aNull) {
            return 1;
        }
        if ($bNull) {
            return -1;
        }
        $c = $natural === 'natural' ? strnatcmp((string)$a, (string)$b) : ($a <=> $b);
        if ($c === 0) {
            return 0;
        }
        return $dir === 'DESC' ? -$c : $c;
    }

    /**
     * 「优选」布尔位，逐项对应 Go 的 livePreferred。
     *
     * 一次节点可用必须同时满足：可连通、出口探针无错、出口 IP 已上报、
     * 连通性与出口都在 45 分钟内（node-liveness 与 node-exit 的周期是 30 分钟，
     * 窗口取 1.5 个周期，否则每个周期里有一半时间优选位恒为 false）、
     * 分类判定为住宅且版本匹配、属性 IP 与出口 IP 一致、分类证据未过期。
     */
    private function isPreferred(array $r, DateTimeImmutable $now): bool
    {
        if (!($r['reachable'] ?? false)) {
            return false;
        }
        if (($r['connectivity_error'] ?? '') !== '') {
            return false;
        }
        if (($r['exit_error'] ?? '') !== '') {
            return false;
        }
        if (($r['reported_exit_ip'] ?? '') === '') {
            return false;
        }
        if (!$this->within($r['connectivity_at'] ?? '', $now, '-45 minutes', '+1 minute')) {
            return false;
        }
        if (!$this->within($r['exit_at'] ?? '', $now, '-45 minutes', '+1 minute')) {
            return false;
        }
        $c = $r['classification'];
        if (!is_object($c)) {
            return false;
        }
        if (($c->state ?? '') !== 'residential') {
            return false;
        }
        $version = (int)config('api.voting_version', 8);
        if ((string)($c->version ?? '') !== (string)$version) {
            return false;
        }
        if (($r['attributes_ip'] ?? '') !== ($r['reported_exit_ip'] ?? '')) {
            return false;
        }
        if (($c->ip ?? '') !== ($r['reported_exit_ip'] ?? '')) {
            return false;
        }
        if (!$this->within($c->checkedAt ?? '', $now, '-24 hours', '+1 minute')) {
            return false;
        }
        $evidence = $c->evidence ?? [];
        if (!is_array($evidence)) {
            return false;
        }
        foreach ($evidence as $e) {
            $ev = is_object($e) ? ($e->checkedAt ?? '') : '';
            if (!$this->within($ev, $now, '-24 hours', '+1 minute')) {
                return false;
            }
        }
        return true;
    }

    /**
     * 时间窗判定：dt 落在 [now+back, now+fwd] 闭区间内。
     *
     * 空串 / 解析失败一律视为不在窗内（等价于 SQL 里的时间戳比较返回 false）。
     */
    private function within(string $ts, DateTimeImmutable $now, string $back, string $fwd): bool
    {
        if ($ts === '') {
            return false;
        }
        $dt = DateTimeImmutable::createFromFormat(DateTimeInterface::RFC3339, $ts)
            ?? DateTimeImmutable::createFromFormat(DateTimeInterface::RFC3339_EXTENDED, $ts);
        if ($dt === false) {
            try {
                $dt = new DateTimeImmutable($ts);
            } catch (Throwable $e) {
                return false;
            }
        }
        return $dt >= $now->modify($back) && $dt <= $now->modify($fwd);
    }

    /** 剥离区域指示符 emoji（🇦–🇿），与 SQL 的 regexp_replace('[🇦-🇿]') 一致。 */
    private function stripEmoji(string $s): string
    {
        return preg_replace('/[\x{1F1E6}-\x{1F1FF}]/u', '', $s) ?? $s;
    }

    /** 全量节点里是否存在某布尔标志（speed_test / media_check）。 */
    private function anyFlag(array $rows, string $key): bool
    {
        foreach ($rows as $r) {
            if (!empty($r[$key])) {
                return true;
            }
        }
        return false;
    }

    /**
     * facets 分桶（对应 SQL 的 facets CTE，作用于全量节点）。
     *
     * @param list<array<string, mixed>> $rows
     * @return list<array{kind:string, value:string, count:int}>
     */
    private function computeFacets(array $rows): array
    {
        $counts = [];
        foreach ($rows as $r) {
            $this->facetAdd($counts, 'country', $r['region']);
            $this->facetAdd($counts, 'type', $r['protocol']);
            $this->facetAdd($counts, 'network', $r['network']);
            if ($r['ip_status'] === 'success') {
                $this->facetAdd($counts, 'ipSource', $r['ip_source']);
            }
            if ($r['tls']) {
                $this->facetAdd($counts, 'features', 'tls');
            }
            if ($r['reality']) {
                $this->facetAdd($counts, 'features', 'reality');
            }
            if ($r['udp']) {
                $this->facetAdd($counts, 'features', 'udp');
            }
            $ipaOk = $r['ip_status'] === 'success' || is_object($r['classification']);
            if ($ipaOk) {
                foreach ($r['ip_attributes'] as $v) {
                    if ($v !== null && $v !== '') {
                        $this->facetAdd($counts, 'ipAttributes', (string)$v);
                    }
                }
            }
            foreach ($r['media'] as $v) {
                if ($v !== null && $v !== '') {
                    $this->facetAdd($counts, 'media', (string)$v);
                }
            }
        }

        $entries = [];
        foreach ($counts as $key => $count) {
            [$kind, $value] = explode('|', $key, 2);
            $entries[] = ['kind' => $kind, 'value' => $value, 'count' => $count];
        }
        return $entries;
    }

    private function facetAdd(array &$counts, string $kind, $value): void
    {
        if ($value === null || $value === '') {
            return;
        }
        $key = $kind . '|' . $value;
        $counts[$key] = ($counts[$key] ?? 0) + 1;
    }

    /** 中位数（percentile_cont(0.5) 语义：偶数个样本取中间两数平均）。 */
    private function median(array $speeds): float
    {
        $n = count($speeds);
        if ($n === 0) {
            return 0.0;
        }
        sort($speeds);
        $rank = 0.5 * ($n - 1);
        $lo = (int)floor($rank);
        $hi = (int)ceil($rank);
        if ($lo === $hi) {
            return (float)$speeds[$lo];
        }
        return (float)($speeds[$lo] + ($rank - $lo) * ($speeds[$hi] - $speeds[$lo]));
    }

    /** 按当页 identity 取 payload（视图按 identity 过滤是高效路径，不会逐行重算）。 */
    private function fetchPayloadsById(array $identities): array
    {
        if ($identities === []) {
            return [];
        }
        $rows = Db::table(self::payloadsTable())
            ->select(['identity', 'payload'])
            ->whereIn('identity', $identities)
            ->get()->all();
        $map = [];
        foreach ($rows as $row) {
            $map[$row->identity] = $this->decodeJson($row->payload);
        }
        return $map;
    }

    /**
     * facets 分桶 + 标签 + 排序。
     *
     * 桶内排序已在 PHP 内完成（count DESC，同 count 按 ICU 数字序用 strnatcmp 兜底），
     * 不再二次排序会打乱既有顺序。features 固定三项：即使 0 个节点也要给出，
     * 否则前端筛选器会塌陷。
     *
     * @param list<array{kind:string,value:string,count:int}> $entries
     * @return array<string, list<array{value:string,label:string,count:int}>>
     */
    public static function facets(array $entries): array
    {
        $buckets = FacetKind::emptyBuckets();
        foreach ($entries as $entry) {
            $kind = FacetKind::tryFrom((string)($entry['kind'] ?? ''));
            if ($kind === null) {
                continue;
            }
            $value = (string)($entry['value'] ?? '');
            $buckets[$kind->value][] = [
                'value' => $value,
                'label' => $kind->label($value),
                'count' => (int)($entry['count'] ?? 0),
            ];
        }

        foreach ($buckets as $kind => $items) {
            if ($kind === 'features') {
                continue;
            }
            usort($items, static fn (array $a, array $b): int =>
                ($b['count'] <=> $a['count']) ?: strnatcmp($a['value'], $b['value']));
            $buckets[$kind] = $items;
        }

        // features 固定三项：即使 0 个节点也要给出，否则前端筛选器会塌陷。
        $counts = array_column($buckets[FacetKind::Features->value], 'count', 'value');
        $buckets[FacetKind::Features->value] = array_map(
            static fn (Feature $feature): array => [
                'value' => $feature->value,
                'label' => $feature->label(),
                'count' => $counts[$feature->value] ?? 0,
            ],
            Feature::cases()
        );

        return $buckets;
    }

    /**
     * 结构化列按关联数组解码（facets 这种简单三元组用）。
     *
     * @return mixed
     */
    private static function decodeJson($value)
    {
        if ($value === null) {
            return null;
        }
        if (is_string($value)) {
            if ($value === 'null') {
                return null;
            }
            return json_decode($value, false, 512, JSON_THROW_ON_ERROR);
        }
        return $value;
    }

    /**
     * pdo_pgsql 对 bool 列的返回形态因版本而异（'t'/'f' 或 true/false）。
     * 直接 (bool)'f' 会得到 true，必须显式判。
     */
    private static function toBool(mixed $value): bool
    {
        if (is_string($value)) {
            return $value !== '' && $value !== 'f' && $value !== 'false' && $value !== '0';
        }
        return (bool)$value;
    }
}
