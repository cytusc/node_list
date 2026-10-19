<?php
declare(strict_types=1);

namespace app\service;

use stdClass;

/**
 * IP 网络类型的投票与标签派生。
 *
 * 移植自 Go 的 backend/internal/ipcheck/voting.go：`Result.Reclassify`、
 * `Result.Attributes`、`Result.summaryType`，以及 `typeLabels`。
 *
 * 为什么接口层需要这份逻辑：Go 的 `/api/results` 不是把 jsonb 原样吐出去，
 * 而是先 `json.Unmarshal` 到 `domain.IPPureResult`，其 UnmarshalJSON 会在
 * 「分类 IP 与出口 IP 一致且带类型证据」时**重算 state 并重写 attributes**。
 * 也就是说响应里的 `classification.state` 与 `attributes` 是**派生值**，
 * 不是库里存的那一份。少了这层，同一份数据两边会输出不同的 state/attributes
 * （例如 US 节点 Go 给 `["residential","isp","住宅IP"]`，只读库就只有前两个）。
 *
 * 投票明细（`Votes`）在 Go 里是 `json:"-"`，不会出现在接口响应里，
 * 因此这里只处理 JSON 中真实存在的 `types`：`summaryType` 里那个
 * 「按 votes 兜底」的分支是给内存调用方的，本实现无需实现。
 */
final class IpClassification
{
    /**
     * 网络类型代码 → 展示标签。
     * 顺序与 Go 的 map 无关（只用于「这个标签算不算类型标签」的判定）。
     */
    private const TYPE_LABELS = [
        'isp' => '住宅IP',
        'business' => '商业宽带',
        'hosting' => '机房IP',
        'mobile' => '移动网络',
        'education' => '教育IP',
        'government' => '政府IP',
        'banking' => '银行网络',
        'organization' => '组织网络',
        'military' => '军事网络',
        'library' => '图书馆网络',
        'cdn' => 'CDN',
        'spider' => '搜索引擎',
        'reserved' => '保留IP',
        'other' => '其他网络',
    ];

    /** 这些标签表达的是「没法判定」，不进 attributes。 */
    private const NOISE_LABELS = ['性质争议', '性质未知', '类型未知', '类型待确认'];

    /**
     * 与 Go 的 IPPureResult.UnmarshalJSON 一致，按类型证据派生当前分类。
     * facts 与详情必须共用此入口；仅复制并改值，不刷新证据或写回原始载荷。
     */
    public static function normalize(stdClass $result): stdClass
    {
        $result = clone $result;
        $classification = $result->classification ?? null;
        if (!$classification instanceof stdClass) {
            return $result;
        }
        $types = $classification->types ?? null;
        if ((string)($classification->ip ?? '') !== (string)($result->ip ?? '')
            || !is_array($types) || $types === []) {
            return $result;
        }
        $classification = clone $classification;
        $summary = self::summaryType($types);
        $classification->state = self::state($summary);
        $result->classification = $classification;
        $result->attributes = self::attributes(
            is_array($result->attributes ?? null) ? array_values($result->attributes) : [],
            $summary
        );
        return $result;
    }

    /**
     * 由类型证据投票得出归一网络类型。
     *
     * 每个 provider 只投一票（usageType 优先、companyType 兜底），
     * 平票时依次偏向 住宅IP > 商业宽带 > 机房IP，无票即「类型未知」。
     *
     * @param array<int, mixed> $types 解码后的 TypeEvidence 列表（stdClass 或关联数组）
     */
    public static function summaryType(array $types): string
    {
        $counts = [];
        $providers = [];
        foreach ($types as $type) {
            $provider = self::field($type, 'provider');
            if (isset($providers[$provider])) {
                continue; // 同一 provider 只认第一条
            }
            $providers[$provider] = true;
            $label = self::field($type, 'usageType');
            if ($label === '') {
                $label = self::field($type, 'companyType');
            }
            if ($label !== '') {
                $counts[$label] = ($counts[$label] ?? 0) + 1;
            }
        }

        $winner = '';
        $best = 0;
        $tied = false;
        foreach ($counts as $label => $count) {
            if ($count > $best) {
                $winner = $label;
                $best = $count;
                $tied = false;
            } elseif ($count === $best) {
                $tied = true;
            }
        }
        if ($best === 0) {
            return '类型未知';
        }
        if ($tied) {
            if (($counts['住宅IP'] ?? 0) === $best) {
                return '住宅IP';
            }
            if (($counts['商业宽带'] ?? 0) === $best) {
                return '商业宽带';
            }
            return '机房IP';
        }
        return (string)$winner;
    }

    /** 归一类型 → 分类状态（Go 的 `Reclassify`，只认这三档）。 */
    public static function state(string $summary): string
    {
        return match ($summary) {
            '住宅IP' => 'residential',
            '机房IP' => 'datacenter',
            default => 'unknown',
        };
    }

    /**
     * 重写 attributes：丢掉「不确定」与类型标签，保留其他标签，末尾补归一类型。
     *
     * @param list<string> $original
     * @return list<string>
     */
    public static function attributes(array $original, string $summary): array
    {
        $out = [];
        $seen = [];
        $append = static function (string $label) use (&$out, &$seen): void {
            if ($label !== '' && !isset($seen[$label])) {
                $out[] = $label;
                $seen[$label] = true;
            }
        };

        foreach ($original as $label) {
            $label = (string)$label;
            if (in_array($label, self::NOISE_LABELS, true)) {
                continue;
            }
            if (in_array($label, self::TYPE_LABELS, true)) {
                continue; // 类型标签由末尾的归一结论统一表达
            }
            $append($label);
        }
        $append($summary);
        return $out;
    }

    /**
     * 读一个字符串字段。
     *
     * 载荷来自 jsonb，解码时保留了对象语义（stdClass），测试里也可能直接喂数组，
     * 所以两种形态都要认。用 `??` + 强转会把「不存在」与「空串」并到一起，
     * 而这正是 Go 里 `""` 与缺省的等价语义，符合预期。
     */
    private static function field(mixed $row, string $key): string
    {
        if ($row instanceof \stdClass) {
            return (string)($row->$key ?? '');
        }
        if (is_array($row)) {
            return (string)($row[$key] ?? '');
        }
        return '';
    }
}
