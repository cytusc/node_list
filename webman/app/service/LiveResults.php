<?php
declare(strict_types=1);

namespace app\service;

use app\service\enum\FacetKind;
use app\service\enum\Feature;
use app\service\enum\SortKey;
use stdClass;
use support\Db;
use support\Log;
use Throwable;

/**
 * 实时结果视图（node_explorer_live_*）的读取实现。
 *
 * 一条 SQL 在同一个数据库快照里完成「元数据 + facets + 过滤 + 分页 + 载荷选取」。
 * 这个结构不是随意拆的，Go 侧的注释记录了踩坑过程，这里原样保留：
 * 如果把 node_explorer_live_nodes 放进 JOIN 的内侧按主键逐行取值，视图会被
 * 逐行重算（生产实测 limit=50 时 12s/请求）。所以必须先用 MATERIALIZED CTE
 * 把过滤/排序收敛成一份行号表，再 JOIN 一次取 payload。
 *
 * PHP 侧的表达方式：
 *   - 条件用 Condition 值对象组合（片段 + 参数成对），不在函数间传引用数组
 *   - 排序键 / 布尔筛选 / 分桶全部走枚举，SQL 里没有一处用户输入
 *   - 只有「必须一条语句一个快照」的部分保留手写 SQL —— 这正是 ORM 表达不了的
 */
final class LiveResults
{
    /** 默认排序：快照倒序 + 节点下标 + 身份，保证同分排序稳定。 */
    private const DEFAULT_ORDER = 'snapshot_id DESC,node_index,identity';

    /**
     * 请求级共享 CTE：只取「过滤 / facet / 排序」需要的列。
     * 完整公共节点文档（payload）留到选出当页之后再取。
     *
     * 与 Go 的 liveRows 逐行一致，改动前请先比对两边的 EXPLAIN。
     */
    private const NODES_CTE = <<<'SQL'
WITH nodes AS MATERIALIZED (
 SELECT identity,snapshot_id,node_index,country,
 coalesce(nullif(country,''),'unknown') AS region,
 node->>'name' AS name,node->>'baseName' AS base_name,node->>'server' AS server,node->>'sni' AS sni,
 node->>'type' AS node_type,
 coalesce(nullif(node->>'type',''),'unknown') AS protocol,
 coalesce(nullif(node->>'network',''),'unknown') AS network,
 coalesce((node->>'speed')::double precision,0) AS speed,
 coalesce((node->>'tls')::boolean,false) AS tls,
 coalesce((node->>'reality')::boolean,false) AS reality,
 coalesce((node->>'udp')::boolean,false) AS udp,
 (node->>'speedTest')::boolean AS speed_test,(node->>'mediaCheck')::boolean AS media_check,
 node->>'checkedAt' AS checked_at,
 attributes->>'ip' AS attributes_ip,ip_status,attributes->>'source' AS ip_source,attributes->>'asnOrganization' AS asn_organization,
 (attributes->>'score')::double precision AS ip_score,(attributes->>'humanPercent')::double precision AS human_percent,
 attributes->'classification' AS classification,
 ARRAY(SELECT jsonb_array_elements_text(coalesce(nullif(attributes->'attributes','null'::jsonb),'[]'::jsonb))) AS ip_attributes,
 coalesce((connectivity->>'reachable')::boolean,false) AS reachable,
 connectivity->>'error' AS connectivity_error,connectivity->>'checkedAt' AS connectivity_at,
 exit_payload->>'ip' AS reported_exit_ip,exit_payload->>'error' AS exit_error,exit_payload->>'checkedAt' AS exit_at,
 ARRAY(SELECT DISTINCT coalesce(nullif(m->>'platform',''),nullif(m->>'Platform',''),nullif(m->>'tag',''),m->>'Tag')
 FROM jsonb_array_elements(coalesce(nullif(node->'media','null'::jsonb),'[]'::jsonb)) m
 WHERE coalesce(nullif(m->>'tag',''),nullif(m->>'Tag',''),nullif(m->>'platform',''),m->>'Platform','')<>'') AS media
 FROM node_explorer_live_facts
)
SQL;

    /**
     * 执行一次结果查询。
     *
     * @param ResultsQuery|null $q null 表示「不分页、不过滤，整份返回」
     * @return array<string, mixed> 响应体（不含 ippureCollector / sourceWarning）
     * @throws ApiError 数据库读失败 → 503 READ_FAILED
     */
    public function query(?ResultsQuery $q): array
    {
        [$sql, $bindings] = $this->build($q);
        try {
            $row = Db::selectOne($sql, $bindings);
        } catch (Throwable $e) {
            // 与 Go 一致：只记录，不把 SQL 细节透给前端。
            Log::error('live results query failed: ' . $e->getMessage());
            throw ApiError::readFailed();
        }
        if (!$row instanceof stdClass) {
            throw ApiError::readFailed();
        }
        return $this->assemble($row);
    }

    /**
     * 拼装 SQL 与绑定参数。
     *
     * 参数顺序 = 条件顺序 + 分页参数：分页占位符出现在 SQL 靠后的 page_options 里，
     * 必须排在所有过滤参数之后，否则绑定错位（Condition 值对象保证了前半段有序）。
     *
     * @return array{0:string, 1:list<mixed>}
     */
    public function build(?ResultsQuery $q): array
    {
        [$where, $bindings] = Condition::merge($q === null ? [] : $this->filters($q));

        $order = self::DEFAULT_ORDER;
        $options = 'SELECT 0::bigint AS page,0::bigint AS page_limit';
        $paging = '';

        if ($q !== null) {
            $options = 'SELECT least(?::bigint,greatest(1,(total+?::bigint-1)/?::bigint)) AS page,?::bigint AS page_limit FROM tally';
            $paging = ' LIMIT (SELECT page_limit FROM page_options) OFFSET (SELECT (page-1)*page_limit FROM page_options)';
            // 页码越界时夹到最后一页（Go 的 least/greatest 语义）。
            $bindings[] = $q->page;
            $bindings[] = $q->limit;
            $bindings[] = $q->limit;
            $bindings[] = $q->limit;
            $order = $this->order($q);
        }

        $sql = self::NODES_CTE . <<<SQL
, metadata AS (
 SELECT count(*) AS total_nodes,count(DISTINCT nullif(country,'')) AS countries,
 coalesce(max(speed) FILTER(WHERE speed>0),0) AS max_speed,
 coalesce(percentile_cont(0.5) WITHIN GROUP(ORDER BY speed) FILTER(WHERE speed>0),0) AS median,
 coalesce(bool_or(speed_test),false) AS speed_test,
 coalesce(bool_or(media_check),false) AS media_check,
 coalesce(max(checked_at),'') AS checked_at FROM nodes
), facets AS (
 SELECT kind,value,count(*) AS count FROM nodes CROSS JOIN LATERAL (
 VALUES ('country',region),('type',protocol),('network',network),
 ('ipSource',CASE WHEN ip_status='success' THEN nullif(ip_source,'') END),
 ('features',CASE WHEN tls THEN 'tls' END),('features',CASE WHEN reality THEN 'reality' END),('features',CASE WHEN udp THEN 'udp' END)
 UNION ALL SELECT 'ipAttributes',value FROM (SELECT DISTINCT unnest(ip_attributes) AS value) a
 WHERE ip_status='success' OR jsonb_typeof(classification)='object'
 UNION ALL SELECT 'media',unnest(media)
 ) f(kind,value) WHERE value IS NOT NULL AND value<>'' GROUP BY kind,value
), filtered AS MATERIALIZED (SELECT * FROM nodes{$where}),
tally AS (SELECT count(*) AS total FROM filtered),
page_options AS ({$options}),
page_candidates AS MATERIALIZED (SELECT identity,row_number() OVER(ORDER BY {$order}) AS ordinal FROM filtered ORDER BY {$order}{$paging}),
page_nodes AS (SELECT v.payload,p.ordinal FROM page_candidates p JOIN node_explorer_live_nodes v USING(identity))
SELECT total_nodes,countries,max_speed,median,speed_test,media_check,checked_at,
 (SELECT coalesce(jsonb_agg(jsonb_build_object('kind',kind,'value',value,'count',count) ORDER BY count DESC,value COLLATE node_explorer_numeric),'[]'::jsonb) FROM facets) AS facets,
 (SELECT total FROM tally) AS total,(SELECT page FROM page_options) AS page,(SELECT page_limit FROM page_options) AS page_limit,
 (SELECT coalesce(jsonb_agg(payload ORDER BY ordinal),'[]'::jsonb) FROM page_nodes) AS nodes
 FROM metadata
SQL;

        return [$sql, $bindings];
    }

    /**
     * ORDER BY 生成。
     *
     * 分支穷尽且互斥：优选位 / 速度特例 / 枚举列 / 缺省。
     * speed 单独一支，是因为没跑过测速的批次里速度没有比较意义，要退回名称排序
     * （与 Go 的 liveSort 分支一致）。
     */
    private function order(ResultsQuery $q): string
    {
        $name = SortKey::nameExpression();
        $direction = $q->direction->sql();
        $speedFirst = 'CASE WHEN (SELECT speed_test FROM metadata) THEN speed END ';

        // 显式 preferred 与「未指定排序」共用同一段「优选优先」排序（与 Go 的
        // case "preferred" / case "" 合并成一支等价）。
        if ($q->preferred || $q->sort === null) {
            return $this->preferredExpression() . ' DESC NULLS LAST,'
                . $speedFirst . 'DESC NULLS LAST,'
                . 'CASE WHEN NOT (SELECT speed_test FROM metadata) THEN ' . $name . ' END ASC NULLS LAST,'
                . self::DEFAULT_ORDER;
        }

        if ($q->sort === SortKey::Speed) {
            return $speedFirst . $direction . ' NULLS LAST,'
                . 'CASE WHEN NOT (SELECT speed_test FROM metadata) THEN ' . $name . ' END ASC NULLS LAST,'
                . self::DEFAULT_ORDER;
        }

        return $q->sort->sqlExpression() . ' ' . $direction . ' NULLS LAST,' . self::DEFAULT_ORDER;
    }

    /**
     * 把筛选条件翻译成 Condition 列表。
     *
     * @return list<Condition>
     */
    private function filters(ResultsQuery $q): array
    {
        $conditions = [];

        if ($q->countries !== []) {
            $conditions[] = Condition::inList('region', $q->countries);
        }
        if ($q->types !== []) {
            $conditions[] = Condition::inList('protocol', $q->types);
        }
        if ($q->networks !== []) {
            $conditions[] = Condition::inList('network', $q->networks);
        }
        foreach ($q->features as $feature) {
            // 列名来自枚举，值域封闭，无注入面。
            $conditions[] = Condition::raw($feature->column());
        }

        if ($q->search !== '') {
            // 关键词命中两类：基础字段子串，或「中文地区名 → 地区代码」。
            $codes = Country::codesMatching($q->search);
            $codesSql = $codes === []
                ? 'ARRAY[]::text[]'
                : 'ARRAY[' . implode(',', array_fill(0, count($codes), '?')) . ']::text[]';
            $conditions[] = new Condition(
                '(EXISTS(SELECT 1 FROM unnest(ARRAY[name,base_name,server,sni,country]) v WHERE strpos(lower(v),?)>0)'
                . ' OR region = ANY(' . $codesSql . '))',
                [$q->search, ...$codes]
            );
        }

        if ($q->ipSources !== []) {
            $conditions[] = Condition::raw("ip_status='success'")
                ->and(Condition::inList('ip_source', $q->ipSources));
        }
        if ($q->ipAttributes !== []) {
            $conditions[] = Condition::raw("(ip_status='success' OR jsonb_typeof(classification)='object')")
                ->and(new Condition(
                    'ip_attributes && ARRAY[' . implode(',', array_fill(0, count($q->ipAttributes), '?')) . ']::text[]',
                    array_values($q->ipAttributes)
                ));
        }
        if ($q->minSpeed > 0) {
            $conditions[] = new Condition(
                '(NOT (SELECT speed_test FROM metadata) OR speed>=?)',
                [$q->minSpeed * 1024]
            );
        }
        if ($q->media !== []) {
            $conditions[] = new Condition(
                '(NOT (SELECT media_check FROM metadata) OR ARRAY['
                . implode(',', array_fill(0, count($q->media), '?')) . ']::text[] <@ media)',
                array_values($q->media)
            );
        }

        return $conditions;
    }

    /**
     * 「优选」布尔位，逐项对应 Go 的 livePreferred。
     *
     * 一次节点可用必须同时满足：可连通、出口探针无错、出口 IP 已上报、
     * 连通性与出口都在 10 分钟内、分类判定为住宅且版本匹配、
     * 属性 IP 与出口 IP 一致、分类证据未过期。
     */
    private function preferredExpression(): string
    {
        $version = (int)config('api.voting_version', 8);
        return <<<SQL
(reachable
 AND coalesce(connectivity_error,'')='' AND coalesce(exit_error,'')=''
 AND coalesce(reported_exit_ip,'')<>''
 AND connectivity_at::timestamptz BETWEEN now()-interval '10 minutes' AND now()+interval '1 minute'
 AND exit_at::timestamptz BETWEEN now()-interval '10 minutes' AND now()+interval '1 minute'
 AND classification->>'state'='residential'
 AND classification->>'version'='{$version}'
 AND attributes_ip=reported_exit_ip
 AND classification->>'ip'=reported_exit_ip
 AND (classification->>'checkedAt')::timestamptz BETWEEN now()-interval '24 hours' AND now()+interval '1 minute'
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(coalesce(nullif(classification->'evidence','null'::jsonb),'[]'::jsonb)) e
 WHERE nullif(e->>'checkedAt','') IS NULL OR NOT((e->>'checkedAt')::timestamptz BETWEEN now()-interval '24 hours' AND now()+interval '1 minute')))
SQL;
    }

    /**
     * 把一行查询结果组装成响应体。
     *
     * 字段顺序与 Go 的 resultsResponse 一致（Metadata 嵌入 → nodes/total/page/limit）；
     * ippureCollector / sourceWarning 由控制器补齐（它们是附属信息，不是结果本身）。
     *
     * @return array<string, mixed>
     */
    private function assemble(stdClass $row): array
    {
        $body = [
            'checkedAt' => (string)($row->checked_at ?? ''),
            'speedTest' => self::toBool($row->speed_test ?? false),
            'mediaCheck' => self::toBool($row->media_check ?? false),
            'totalNodes' => (int)($row->total_nodes ?? 0),
            'summary' => [
                'countries' => (int)($row->countries ?? 0),
                'maxSpeed' => JsonResponse::number($row->max_speed ?? 0),
                'median' => JsonResponse::number($row->median ?? 0),
            ],
            // facets 是简单三元组，用关联数组解码即可（nodes 必须保留对象语义，见下）。
            'facets' => self::facets(self::decodeList($row->facets ?? '[]')),
            'nodes' => NodePresenter::nodes(self::decodeJson($row->nodes ?? '[]')),
            'total' => (int)($row->total ?? 0),
            'page' => (int)($row->page ?? 0),
            'limit' => (int)($row->page_limit ?? 0),
        ];

        // Go 的 Metadata.CheckedAt 带 omitempty：空值不输出。
        if ($body['checkedAt'] === '') {
            unset($body['checkedAt']);
        }
        return $body;
    }

    /**
     * facets 分桶 + 标签。
     *
     * 排序已在 SQL 内完成（count DESC, ICU 数字序），这里只做分桶与标签，
     * 不再二次排序 —— 二次排序会打乱「同 count 时按 value」的既有顺序。
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
     * @return list<array<string, mixed>>
     */
    private static function decodeList(mixed $value): array
    {
        if (is_string($value)) {
            $value = json_decode($value, true, 512, JSON_THROW_ON_ERROR);
        }
        return is_array($value) ? $value : [];
    }

    /** jsonb 列在 PDO 里是字符串；已解码时原样返回。 */
    private static function decodeJson(mixed $value): mixed
    {
        if (is_string($value)) {
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
