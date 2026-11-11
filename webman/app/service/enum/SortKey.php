<?php
declare(strict_types=1);

namespace app\service\enum;

/** 节点排序参数白名单及数值类型。 */
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

}
