<?php
declare(strict_types=1);

namespace app\service;

final class OpenNodesQuery
{
    public const PAGE_SIZE = 10;

    public function __construct(
        public readonly string $country,
        public readonly int $page = 1,
    ) {}

    /** @param array<string, string> $params */
    public static function fromParameters(array $params): self
    {
        if (array_diff(array_keys($params), ['country', 'mode', 'page']) !== []) self::invalid();
        $country = strtoupper($params['country'] ?? '');
        if (!preg_match('/^[A-Z]{2}$/D', $country) || $country === 'ZZ') self::invalid();
        if (($params['mode'] ?? 'standard') !== 'standard') {
            throw new ApiError(400, '仅支持 standard 模式。', 'INVALID_MODE');
        }
        $page = $params['page'] ?? '1';
        if (!preg_match('/^[1-9][0-9]{0,9}$/D', $page) || (int)$page > 2147483647) self::invalid();
        return new self($country, (int)$page);
    }

    private static function invalid(): never
    {
        throw new ApiError(400, '请提供有效的 country 和正整数 page，每页固定 10 条。', 'INVALID_QUERY');
    }
}
