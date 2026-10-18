<?php
declare(strict_types=1);

namespace app\service\enum;

/**
 * 排序方向。
 *
 * 用枚举替代 'asc'/'desc' 字符串：请求校验、SQL 拼接、默认值都收敛到一处，
 * 拼 SQL 时必须显式判方向，杜绝把用户输入直接内联进 ORDER BY。
 */
enum Direction: string
{
    case Asc = 'asc';
    case Desc = 'desc';

    /** 缺省方向。结果页默认「大的/新的在前」，与前端表格初始状态一致。 */
    public const DEFAULT = self::Desc;

    public static function fromQuery(string $raw): self
    {
        return $raw === '' ? self::DEFAULT : (self::tryFrom($raw) ?? throw new \ValueError('invalid direction'));
    }

    /** SQL 关键字（枚举值的封闭集合，可安全内联）。 */
    public function sql(): string
    {
        return match ($this) {
            self::Asc => 'ASC',
            self::Desc => 'DESC',
        };
    }
}
