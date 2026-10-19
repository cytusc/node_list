<?php
declare(strict_types=1);

namespace app\service\enum;

use app\service\Country;

/**
 * facets 的分桶类型。
 *
 * SQL 只吐出 `(kind, value, count)` 三元组，标签规则留在 PHP 侧：
 * 把「kind → 标签怎么算」收进枚举后，`LiveResults::facets()` 里那串
 * switch 就退化成一次 `FacetKind::tryFrom()->label()`，新增分桶只改这里。
 *
 * 枚举值必须与 SQL 里 VALUES 列表的字符串逐字一致（测试会比对）。
 */
enum FacetKind: string
{
    case Country = 'country';
    case Type = 'type';
    case Network = 'network';
    case IPSource = 'ipSource';
    case IPAttributes = 'ipAttributes';
    case Features = 'features';
    case Media = 'media';

    /**
     * 取值 → 展示标签。
     *
     * - country：ISO 代码翻中文名（未知代码原样返回）
     * - type / network：协议名统一大写
     * - 其余：标签即原值，前端自己映射图标/名称
     */
    public function label(string $value): string
    {
        return match ($this) {
            self::Country => Country::name($value),
            self::Type, self::Network => strtoupper($value),
            default => $value,
        };
    }

    /** 空桶的初始结构：响应里每个分桶都必须存在，前端才能无条件取用。 */
    public static function emptyBuckets(): array
    {
        $buckets = [];
        foreach (self::cases() as $case) {
            $buckets[$case->value] = [];
        }
        return $buckets;
    }
}
