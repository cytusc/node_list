<?php
declare(strict_types=1);

namespace app\service;

use app\service\enum\Direction;
use app\service\enum\Feature;
use app\service\enum\SortKey;

/**
 * 一次结果查询的解析结果（不可变）。
 *
 * 与 Go 的 postgres.Query 字段一一对应，但有两处刻意的类型收紧：
 *
 *   - `sort` 是 `?SortKey`，`sort=preferred` 单独用 `$preferred` 布尔表达。
 *     Go 版用空串 + "preferred" 字符串混在同一个字段里，下游必须反复判字符串；
 *     这里把「不指定列」与「按列排序」在类型层面分开，SQL 分支变成穷尽匹配。
 *   - `features` 是 `list<Feature>`，`direction` 是 `Direction`。
 *     白名单校验在解析阶段一次完成，SQL 层拿到的值不可能越界。
 *
 * 之所以保留 `?` 表示「未分页」：契约允许不带 page/limit 请求整份视图
 * （前端首屏不用），用 null 表达比用 page=0 哨兵更清楚。
 */
final class ResultsQuery
{
    /**
     * @param list<string>  $countries
     * @param list<string>  $types
     * @param list<string>  $networks
     * @param list<Feature> $features
     * @param list<string>  $ipSources
     * @param list<string>  $ipAttributes
     * @param list<string>  $media
     * @param float         $minSpeed Mbps，SQL 侧乘 1024 换成节点记录里的字节速度
     */
    public function __construct(
        public readonly int $page,
        public readonly int $limit,
        public readonly string $search = '',
        public readonly array $countries = [],
        public readonly array $types = [],
        public readonly array $networks = [],
        public readonly array $features = [],
        public readonly array $ipSources = [],
        public readonly array $ipAttributes = [],
        public readonly array $media = [],
        public readonly float $minSpeed = 0.0,
        public readonly ?SortKey $sort = null,
        public readonly bool $preferred = false,
        public readonly Direction $direction = Direction::Desc,
    ) {
    }
}
