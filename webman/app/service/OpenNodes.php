<?php
declare(strict_types=1);

namespace app\service;

use support\Db;
use support\Log;
use Throwable;

/** 对外节点查询：SQL 完成筛选、标准排序和分页，只投影约定的对外字段。 */
final class OpenNodes
{
    /** 国家统计与节点查询共用发布视图、地区校验和非空链接条件。 */
    private const ELIGIBLE = <<<'SQL'
        SELECT f.identity, upper(f.country) AS country, f.node, f.exit_ip,
          CASE WHEN nullif(btrim(f.node->>'rawLink'),'') IS NOT NULL
            THEN f.node->>'rawLink' ELSE f.node->>'generatedLink' END AS link,
          CASE WHEN f.attributes->>'ip'=f.exit_ip
            AND coalesce(f.attributes->>'stale','false')='false'
            THEN f.attributes END AS attributes
        FROM node_explorer_live_facts f
        WHERE upper(f.country) ~ '^[A-Z]{2}$' AND upper(f.country)<>'ZZ'
          AND coalesce(nullif(btrim(f.node->>'rawLink'),''),nullif(btrim(f.node->>'generatedLink'),'')) IS NOT NULL
        SQL;

    /** @return list<array{code:string, name:string, node_count:int}> */
    public function countries(): array
    {
        try {
            $rows = Db::select('WITH eligible AS (' . self::ELIGIBLE . ')
                SELECT country, count(*) AS total FROM eligible GROUP BY country ORDER BY country COLLATE "C"');
            return array_map(static fn ($r): array => [
                'code' => $r->country,
                'name' => Country::name($r->country),
                'node_count' => (int)$r->total,
            ], $rows);
        } catch (Throwable $e) {
            Log::error('open api country query failed');
            throw ApiError::readFailed();
        }
    }

    public function nodes(OpenNodesQuery $query): array
    {
        try {
            return Db::connection()->transaction(function () use ($query): array {
                if (Db::connection()->transactionLevel() === 1) {
                    Db::statement('SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY');
                }
                $eligible = self::ELIGIBLE . ' AND upper(f.country)=?';
                $total = (int)Db::selectOne('SELECT count(*) AS total FROM (' . $eligible . ') eligible', [$query->country])->total;
                $offset = ($query->page - 1) * OpenNodesQuery::PAGE_SIZE;
                $rows = [];
                if ($offset < $total) {
                    $rows = Db::select('WITH eligible AS (' . $eligible . '), ' . self::STANDARD, [
                        $query->country,
                        (string)config('api.voting_version', 8),
                        OpenNodesQuery::PAGE_SIZE,
                        $offset,
                    ]);
                }
                return [
                    'country' => $query->country,
                    'mode' => 'standard',
                    'page' => $query->page,
                    'page_size' => OpenNodesQuery::PAGE_SIZE,
                    'total' => $total,
                    'has_more' => $offset + count($rows) < $total,
                    'nodes' => array_map(self::present(...), $rows),
                ];
            });
        } catch (Throwable $e) {
            Log::error('open api node query failed');
            throw ApiError::readFailed();
        }
    }

    // 类型投票逐项对应 IpClassification::summaryType：每个 provider 第一票，
    // usageType 优先于 companyType，最多票平局按住宅、商业、机房处理。
    // JSON 数值仅接受 number 且在 [0,100]；缺失值保留 NULL，绝不变成 0。
    private const STANDARD = <<<'SQL'
        prepared AS (
          SELECT e.*,
            CASE WHEN attributes->'classification'->>'ip'=exit_ip
              AND attributes->'classification'->>'version'=?
              AND jsonb_typeof(attributes->'classification'->'types')='array'
              THEN attributes->'classification'->'types' ELSE '[]'::jsonb END AS types,
            CASE WHEN attributes->>'status'='success' AND jsonb_typeof(attributes->'score')='number'
              THEN (attributes->>'score')::numeric END AS score_value,
            CASE WHEN coalesce(attributes->>'trafficRatioStatus','') IN ('','success')
              AND jsonb_typeof(attributes->'humanPercent')='number'
              THEN (attributes->>'humanPercent')::numeric END AS human_value,
            CASE WHEN coalesce(attributes->>'trafficRatioStatus','') IN ('','success')
              AND jsonb_typeof(attributes->'botPercent')='number'
              THEN (attributes->>'botPercent')::numeric END AS bot_value,
            CASE WHEN attributes->>'status'='success'
              AND attributes->>'source' IN ('原生IP','广播IP') THEN attributes->>'source' END AS ip_source
          FROM eligible e
        ), ranked AS (
          SELECT p.*,
            CASE WHEN score_value BETWEEN 0 AND 100 THEN score_value END AS risk_score,
            CASE WHEN human_value BETWEEN 0 AND 100 THEN human_value END AS human_percent,
            CASE WHEN bot_value BETWEEN 0 AND 100 THEN bot_value END AS bot_percent,
            c.network_type
          FROM prepared p
          LEFT JOIN LATERAL (
            WITH first_votes AS (
              SELECT DISTINCT ON (coalesce(t.item->>'provider',''))
                coalesce(t.item->>'provider','') AS provider,
                coalesce(nullif(t.item->>'usageType',''),t.item->>'companyType','') AS label
              FROM jsonb_array_elements(p.types) WITH ORDINALITY AS t(item, position)
              ORDER BY coalesce(t.item->>'provider',''), t.position
            ), votes AS (
              SELECT label, count(*) AS total FROM first_votes WHERE label<>'' GROUP BY label
            ), leaders AS (
              SELECT label FROM votes WHERE total=(SELECT max(total) FROM votes)
            )
            SELECT CASE WHEN count(*)=0 THEN NULL
              WHEN count(*)=1 THEN min(label)
              WHEN bool_or(label='住宅IP') THEN '住宅IP'
              WHEN bool_or(label='商业宽带') THEN '商业宽带'
              ELSE '机房IP' END AS network_type FROM leaders
          ) c ON true
        )
        SELECT identity, node->>'name' AS name, node->>'type' AS type,
          country, exit_ip, link, network_type, risk_score, human_percent, bot_percent, ip_source
        FROM ranked
        ORDER BY
          CASE WHEN network_type='住宅IP' THEN 0
            WHEN network_type IS NOT NULL AND network_type NOT IN ('类型未知','性质未知','性质争议','类型待确认') THEN 1 ELSE 2 END,
          risk_score ASC NULLS LAST,
          CASE WHEN human_percent IS NULL OR bot_percent IS NULL THEN 2
            WHEN human_percent>bot_percent THEN 0 ELSE 1 END,
          CASE ip_source WHEN '原生IP' THEN 0 WHEN '广播IP' THEN 1 ELSE 2 END,
          identity COLLATE "C" ASC
        LIMIT ? OFFSET ?
        SQL;

    private static function present(object $row): array
    {
        $type = $row->network_type;
        $residential = $type === null || in_array($type, ['类型未知', '性质未知', '性质争议', '类型待确认'], true)
            ? null : $type === '住宅IP';
        $human = $row->human_percent === null ? null : JsonResponse::number($row->human_percent);
        $bot = $row->bot_percent === null ? null : JsonResponse::number($row->bot_percent);
        return [
            'id' => $row->identity,
            'name' => $row->name ?? '',
            'type' => $row->type ?? '',
            'country' => $row->country,
            'exit_ip' => $row->exit_ip,
            'link' => $row->link,
            'network_type' => $type,
            'residential' => $residential,
            'risk_score' => $row->risk_score === null ? null : JsonResponse::number($row->risk_score),
            'human_percent' => $human,
            'bot_percent' => $bot,
            'human_preferred' => $human === null || $bot === null ? null : $human > $bot,
            'native' => $row->ip_source === null ? null : $row->ip_source === '原生IP',
        ];
    }
}
