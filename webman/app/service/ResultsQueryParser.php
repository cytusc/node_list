<?php
declare(strict_types=1);

namespace app\service;

use app\service\enum\Direction;
use app\service\enum\Feature;
use app\service\enum\SortKey;

/**
 * /api/results 的查询参数解析与校验。规则：
 *   1. 既没有 page 也没有 limit → 视为「不分页」，返回 null（取全量节点）
 *   2. page/limit 必须是正整数，limit 上限取 config('api.max_limit')
 *   3. sort 必须是 SortKey 之一，或特例 preferred
 *   4. direction 只允许 asc/desc，缺省 desc
 *   5. minSpeed 必须是非负有限浮点
 *   6. features 只允许 Feature 枚举里的值
 *   7. 其余筛选键按逗号切分，只丢弃空串（不 trim）
 *
 * 任何一条不满足都抛 400「节点分页、筛选或排序参数无效。」—— 文案与状态码
 * 是前端契约，改动等于改前端错误展示。
 *
 * 解析产物是类型化的 ResultsQuery：白名单在这里一次性收口，后面的 SQL 层
 * 只处理枚举与整数，不再有「字符串拼进 SQL」的路径。
 */
final class ResultsQueryParser
{
    public const INVALID_MESSAGE = '节点分页、筛选或排序参数无效。';

    /**
     * @param array<string, mixed> $params 原始 query 参数
     * @return ResultsQuery|null           null 表示「整份视图，不分页」
     * @throws ApiError                    参数不合法
     */
    public static function fromQuery(array $params): ?ResultsQuery
    {
        // 分页参数是「是否按页取」的开关：两个键都没出现时整份返回。
        if (!array_key_exists('page', $params) && !array_key_exists('limit', $params)) {
            return null;
        }

        $page = self::positiveInt(self::raw($params, 'page'), 1);
        $limit = self::positiveInt(
            self::raw($params, 'limit'),
            (int)config('api.default_limit', 50)
        );
        if ($limit > (int)config('api.max_limit', 200)) {
            throw ApiError::invalidQuery(self::INVALID_MESSAGE);
        }

        $sortRaw = (string)self::raw($params, 'sort');
        $preferred = $sortRaw === 'preferred';
        $sortKey = SortKey::tryFromQuery($sortRaw);
        if ($sortRaw !== '' && !$preferred && $sortKey === null) {
            throw ApiError::invalidQuery(self::INVALID_MESSAGE);
        }

        $directionRaw = (string)self::raw($params, 'direction');
        $direction = $directionRaw === '' ? Direction::DEFAULT : Direction::tryFrom($directionRaw);
        if ($direction === null) {
            throw ApiError::invalidQuery(self::INVALID_MESSAGE);
        }

        $minSpeed = self::minSpeed(self::raw($params, 'minSpeed'));

        $features = [];
        foreach (self::splitList((string)self::raw($params, 'features')) as $value) {
            $feature = Feature::tryFrom($value);
            if ($feature === null) {
                throw ApiError::invalidQuery(self::INVALID_MESSAGE);
            }
            $features[] = $feature;
        }

        return new ResultsQuery(
            page: $page,
            limit: $limit,
            // Go: strings.ToLower(strings.TrimSpace(v))
            search: mb_strtolower(trim((string)self::raw($params, 'q')), 'UTF-8'),
            countries: self::splitList((string)self::raw($params, 'country')),
            types: self::splitList((string)self::raw($params, 'type')),
            networks: self::splitList((string)self::raw($params, 'network')),
            features: $features,
            ipSources: self::splitList((string)self::raw($params, 'ipSource')),
            ipAttributes: self::splitList((string)self::raw($params, 'ipAttributes')),
            media: self::splitList((string)self::raw($params, 'media')),
            minSpeed: $minSpeed,
            sort: $sortKey,
            preferred: $preferred,
            direction: $direction,
        );
    }

    /**
     * 取原始值，保留「键不存在」与「值为空串」的区分（对应 url.Values.Has/Get）。
     * 数组参数取首值，与 Go 的 Values.Get 行为一致。
     *
     * @param array<string, mixed> $params
     */
    private static function raw(array $params, string $key): ?string
    {
        if (!array_key_exists($key, $params)) {
            return null;
        }
        $value = $params[$key];
        if (is_array($value)) {
            $value = reset($value);
        }
        if ($value === null || $value === false) {
            return '';
        }
        return is_scalar($value) ? (string)$value : '';
    }

    /**
     * Go 的 positiveInt：空值取默认，其余必须 >=1 的十进制整数。
     * strconv.Atoi 接受前导正负号但不接受空白与下划线，这里用同形式正则。
     */
    private static function positiveInt(?string $raw, int $fallback): int
    {
        if ($raw === null || $raw === '') {
            return $fallback;
        }
        if (!preg_match('/^[+-]?[0-9]+$/', $raw) || (int)$raw < 1) {
            throw ApiError::invalidQuery(self::INVALID_MESSAGE);
        }
        return (int)$raw;
    }

    /**
     * Mbps → 非负有限浮点。
     *
     * 比 Go 更严一点：NaN/Inf 直接拒绝（Go 的 `parsed < 0` 会放过它们，
     * 随后被绑成 PG 的 'NaN' 参与比较，结果恒空且难排查）。
     */
    private static function minSpeed(?string $raw): float
    {
        if ($raw === null || $raw === '') {
            return 0.0;
        }
        if (!is_numeric($raw)) {
            throw ApiError::invalidQuery(self::INVALID_MESSAGE);
        }
        $value = (float)$raw;
        if (!is_finite($value) || $value < 0) {
            throw ApiError::invalidQuery(self::INVALID_MESSAGE);
        }
        return $value;
    }

    /**
     * 逗号切分，只丢弃空串（"a,,b" → ["a","b"]），不做 trim。
     *
     * @return list<string>
     */
    private static function splitList(string $raw): array
    {
        if ($raw === '') {
            return [];
        }
        return array_values(array_filter(explode(',', $raw), static fn (string $part): bool => $part !== ''));
    }
}
