<?php
declare(strict_types=1);

namespace app\service\enum;

/**
 * 结果视图的排序键。
 *
 * 这个枚举同时承担三件事，替代 Go 侧散落在三处的同一份信息
 * （domain.ResultSortKeys 白名单 / projection.SortColumn / live.go 的 liveSort 映射）：
 *
 *   1. 请求校验：`SortKey::tryFromQuery()` 只认枚举里的键，天然没有白名单泄漏
 *   2. SQL 生成：`sqlExpression()` 提供 ORDER BY 表达式，键与列一一绑定，不会错配
 *   3. 前端契约：枚举值就是 `sort` 参数与 `facets` 里出现的字符串
 *
 * 为什么不用数组常量：数组允许任意字符串键混入，改一个键要同步改三处；
 * 枚举让「新增排序维度」变成编译器可查的一次改动（漏实现方法直接报错）。
 */
enum SortKey: string
{
    case Country = 'country';
    case Name = 'name';
    case Type = 'type';
    case Speed = 'speed';
    case AsnOrganization = 'asnOrganization';
    case IPSource = 'ipSource';
    case IPAttributes = 'ipAttributes';
    case IPPure = 'ippure';
    case TrafficRatio = 'trafficRatio';

    /**
     * 解析请求里的 sort 参数。
     *
     * 空串与 "preferred" 是两种「不指定具体列」的形态，由调用方处理，
     * 因此这里返回 null 而不是抛错 —— 校验与语义分开，调用方更好读。
     */
    public static function tryFromQuery(string $raw): ?self
    {
        if ($raw === '' || $raw === 'preferred') {
            return null;
        }
        return self::tryFrom($raw);
    }

    /** 该键是否为数值型（决定排序时按数值还是按 ICU 数字序比较）。 */
    public function isNumeric(): bool
    {
        return match ($this) {
            self::Speed, self::IPPure, self::TrafficRatio => true,
            default => false,
        };
    }

    /**
     * ORDER BY 表达式。
     *
     * Collation node_explorer_numeric 由 live.sql 创建（ICU zh-u-kn），
     * 保证「美国 2」排在「美国 10」之前；换成默认 collation 会让排序退化。
     */
    public function sqlExpression(): string
    {
        return match ($this) {
            self::Country => "nullif(country,'') COLLATE node_explorer_numeric",
            // 名称排序要剥离国旗 emoji，否则同一国家的节点会被 emoji 打乱。
            self::Name => "coalesce(nullif(btrim(regexp_replace(coalesce(nullif(base_name,''),name),'[🇦-🇿]','','g')),''),nullif(name,'')) COLLATE node_explorer_numeric",
            self::Type => "nullif(node_type,'') COLLATE node_explorer_numeric",
            self::Speed => 'speed',
            self::AsnOrganization => "(CASE WHEN ip_status='success' THEN nullif(asn_organization,'') END) COLLATE node_explorer_numeric",
            self::IPSource => "(CASE WHEN ip_status='success' THEN nullif(ip_source,'') END) COLLATE node_explorer_numeric",
            self::IPAttributes => "(CASE WHEN ip_status='success' OR jsonb_typeof(classification)='object' THEN nullif(array_to_string(ip_attributes,' · '),'') END) COLLATE node_explorer_numeric",
            self::IPPure => "CASE WHEN ip_status='success' THEN coalesce(ip_score,0) END",
            self::TrafficRatio => "CASE WHEN ip_status='success' THEN human_percent END",
        };
    }

    /** 名称排序表达式（速度排序未启用时的兜底，与 Go 的 liveSort["name"] 一致）。 */
    public static function nameExpression(): string
    {
        return self::Name->sqlExpression();
    }
}
