<?php
declare(strict_types=1);

namespace app\service\enum;

/**
 * 布尔特性筛选（TLS / Reality / UDP）。
 *
 * 值即 SQL 列名（列名来自本枚举，绝不来自请求），标签即前端筛选器文案。
 * facets 的固定三项顺序也由 `cases()` 决定 —— 不再手写三行字面量数组。
 */
enum Feature: string
{
    case Tls = 'tls';
    case Reality = 'reality';
    case Udp = 'udp';

    /** 过滤条件的 SQL 片段（列名内联，值域受枚举约束，无注入面）。 */
    public function column(): string
    {
        return $this->value;
    }

    /** 前端展示标签。 */
    public function label(): string
    {
        return match ($this) {
            self::Tls => 'TLS',
            self::Reality => 'Reality',
            self::Udp => 'UDP',
        };
    }

    /**
     * @return list<string> 全部合法取值，用于错误信息与测试断言
     */
    public static function values(): array
    {
        return array_map(static fn (self $case): string => $case->value, self::cases());
    }
}
