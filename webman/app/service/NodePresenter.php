<?php
declare(strict_types=1);

namespace app\service;

use stdClass;

/**
 * 节点 JSON 出口规范化。
 *
 * 为什么需要它：Go 的 /api/results 并不是把 jsonb 原文直接吐给浏览器，而是
 * json.Unmarshal 到 domain.Node 再 Marshal。domain.Node 用了自定义的
 * MarshalJSON（internal/domain/jsonshape.go），规则是：
 *
 *   - 键顺序 = 结构体声明顺序（不是 jsonb 的存储顺序）
 *   - 带 omitempty 的字段：值为零时，原本存在就保留（补回零值），原本不存在就删除
 *   - 不带 omitempty 的字段（tls/udp/speed 等）：原本不存在就删除
 *   - 原值为 null 且字段为零值 → 输出 null
 *   - 空数组（[]）落在 nil slice 上 → 输出 null（Go 的 nil slice 序列化行为）
 *   - port 一律字符串化（源文件可能给数字），speed 是 float64
 *
 * 前端依赖的是「字段集合」，缺一个字段就等于「该节点没有这项信息」，
 * 所以这一层不能省，也不能用 jsonb 原样输出替代。
 *
 * 嵌套结构分两类：
 *   - 有 shape 的（Node / IPPureResult）→ renderShaped 走上面的规则
 *   - 普通结构体（Detail / ipcheck.Result / TypeEvidence / Evidence / ExitIPAttempt）
 *     → renderPlain，即标准 encoding/json 语义：非 omitempty 始终输出（含零值），
 *       omitempty 为空则省略
 */
final class NodePresenter
{
    /**
     * domain.Node 的字段表：[字段名, 是否 omitempty, 类型标识]。
     * 顺序必须与 internal/domain/node.go 的 Node 结构体声明顺序一致。
     */
    private const NODE_FIELDS = [
        ['name',          true,  'string'],
        ['baseName',      true,  'string'],
        ['type',          true,  'string'],
        ['server',        true,  'string'],
        ['port',          true,  'string'],
        ['sni',           true,  'string'],
        ['tls',           false, 'bool'],
        ['reality',       true,  'bool'],
        ['udp',           false, 'bool'],
        ['network',       true,  'string'],
        ['speed',         false, 'number'],
        ['country',       true,  'string'],
        ['claimedCountry', true, 'string'],
        ['ip',            true,  'string'],
        ['ipRisk',        true,  'string'],
        ['media',         true,  'media'],
        ['subTag',        true,  'string'],
        ['details',       true,  'details'],
        ['rawLink',       true,  'string'],
        ['rawConfig',     true,  'object'],
        ['ippure',        true,  'ippure'],
        ['connectivity',  true,  'connectivity'],
        ['exit',          true,  'exit'],
        ['lifecycle',     true,  'lifecycle'],
        ['snapshotId',    true,  'number'],
        ['addedAt',       true,  'string'],
        ['checkedAt',     true,  'string'],
        ['speedTest',     true,  'bool'],
        ['mediaCheck',    true,  'bool'],
    ];

    /** domain.IPPureResult 的字段表，顺序同 internal/domain/node.go。 */
    private const IPPURE_FIELDS = [
        ['classification',         true,  'classification'],
        ['checkedAt',              false, 'string'],
        ['attempts',               false, 'number'],
        ['status',                 true,  'string'],
        ['ip',                     true,  'string'],
        ['asn',                    true,  'string'],
        ['asnOrganization',        true,  'string'],
        ['source',                 true,  'string'],
        ['attributes',             true,  'attributes'],
        ['humanPercent',           true,  'number'],
        ['botPercent',             true,  'number'],
        ['trafficRatioStatus',     true,  'string'],
        ['trafficRatioCheckedAt',  true,  'string'],
        ['score',                  true,  'number'],
        ['label',                  true,  'string'],
        ['country',                true,  'string'],
        ['addressFamily',          true,  'number'],
        ['queryTarget',            true,  'string'],
        ['exitIPSource',           true,  'string'],
        ['exitIPMode',             true,  'string'],
        ['exitIPVersion',          true,  'number'],
        ['exitIPProbeVersion',     true,  'number'],
        ['queryMode',              true,  'string'],
        ['transportMode',          true,  'string'],
        ['signatureVersion',       true,  'number'],
        ['failureStage',           true,  'string'],
        ['shareLinkVersion',       true,  'number'],
        ['error',                  true,  'string'],
        ['exitIPAttempts',         true,  'attempts'],
        ['organizationAttempts',   true,  'number'],
        ['organizationCheckedAt',  true,  'string'],
        ['githubReachable',        true,  'bool'],
    ];

    /**
     * domain.ConnectivityState。
     * 注意这层不是可选优化：Go 把它解码成结构体再编码，所以 jsonb 里的
     * 未知键会消失、空串 error 会被 omitempty 省掉、键序回到声明顺序。
     * 直接透传 jsonb 会多出键、少掉顺序，逐字节比对立刻红。
     */
    private const CONNECTIVITY_FIELDS = [
        ['checkedAt',     false, 'string'],
        ['reachable',     false, 'bool'],
        ['confirmedDead', true,  'bool'],
        ['error',         true,  'string'],
    ];

    /** domain.ExitState：version / verified 没有 omitempty，零值也照出。 */
    private const EXIT_FIELDS = [
        ['ip',            true,  'string'],
        ['checkedAt',     false, 'string'],
        ['version',       false, 'number'],
        ['source',        true,  'string'],
        ['country',       true,  'string'],
        ['addressFamily', true,  'number'],
        ['verified',      false, 'bool'],
        ['error',         true,  'string'],
    ];

    /** domain.LifecycleState：live 视图固定给 {"status":"active"}。 */
    private const LIFECYCLE_FIELDS = [
        ['status',    false, 'string'],
        ['retiredAt', true,  'string'],
        ['reason',    true,  'string'],
    ];

    /**
     * 把一页节点的原始 jsonb 载荷规范化为可输出的数组。
     *
     * @param list<stdClass|array<string,mixed>> $nodes node_explorer_live_nodes.payload 解码结果
     * @return list<array<string,mixed>>
     */
    public static function nodes(array $nodes): array
    {
        $out = [];
        foreach ($nodes as $node) {
            $out[] = self::publicNode(self::renderShaped(self::asObject($node), self::NODE_FIELDS));
        }
        return $out;
    }

    /** 单个节点（供单元测试直接调用）。 */
    public static function node(stdClass|array $node): array
    {
        return self::publicNode(self::renderShaped(self::asObject($node), self::NODE_FIELDS));
    }

    /**
     * 剥离凭证字段，对应 Go 的 Node.ClearCredentials + api.publicSnapshot。
     *
     * 纵深防御：live 视图在 SQL 里已经 `- 'rawLink' - 'rawConfig'`，这里再兜一次，
     * 避免将来视图改动或换数据源时把订阅链接/完整配置漏给浏览器。
     * 原始链接等于账号凭证，泄露面比「少一个字段」严重得多。
     *
     * @param array<string, mixed> $node
     * @return array<string, mixed>
     */
    private static function publicNode(array $node): array
    {
        unset($node['rawLink'], $node['rawConfig']);
        return $node;
    }

    /**
     * 复刻 marshalJSONShape：按字段表顺序输出，并处理 present / null / empty 三种形态。
     *
     * @param array<int, array{0:string,1:bool,2:string}> $fields
     * @return array<string, mixed>
     */
    private static function renderShaped(stdClass $payload, array $fields): array
    {
        $out = [];
        foreach ($fields as [$name, $omitEmpty, $kind]) {
            $present = property_exists($payload, $name);
            $raw = $present ? $payload->$name : null;
            $isNull = $present && $raw === null;

            if (!self::isEmpty($raw)) {
                $out[$name] = self::convert($name, $kind, $raw);
                continue;
            }

            // 与 Go 一致：只有「需要特别处理」的形态才动字段，否则交给标准编码器。
            $changed = ($present && $omitEmpty)
                || (!$present && !$omitEmpty)
                || $isNull;

            if (!$changed) {
                if (!$omitEmpty) {
                    // 非 omitempty 的空值：标准编码器输出零值。
                    $out[$name] = self::zero($kind);
                }
                continue;
            }
            if (!$present) {
                // 原载荷没有这个键，且字段是非 omitempty → 删掉它。
                continue;
            }
            // 原载荷有这个键：null 保持 null，否则补回零值（nil slice → null）。
            $out[$name] = $isNull ? null : self::zero($kind);
        }
        return $out;
    }

    /**
     * 普通结构体语义：非 omitempty 始终出现，omitempty 为空时省略。
     *
     * @param array<int, array{0:string,1:bool,2:string}> $fields
     * @return array<string, mixed>
     */
    private static function renderPlain(stdClass $payload, array $fields): array
    {
        $out = [];
        foreach ($fields as [$name, $omitEmpty, $kind]) {
            $present = property_exists($payload, $name);
            $raw = $present ? $payload->$name : null;
            // *bool 的空值是 nil，而不是 false：hosting=false 是一条有效证据，不能省。
            $empty = $kind === 'nullableBool' ? $raw === null : self::isEmpty($raw);
            if ($omitEmpty && $empty) {
                continue;
            }
            $out[$name] = $present ? self::convert($name, $kind, $raw) : self::zero($kind);
        }
        return $out;
    }

    /** 按类型标识转换值（含嵌套结构）。 */
    private static function convert(string $name, string $kind, mixed $value): mixed
    {
        return match ($kind) {
            // 端口在 domain.Node 里是 string，源文件给数字时必须字符串化。
            'string' => is_string($value) ? $value : (string)self::scalarString($value),
            'bool' => (bool)$value,
            'number' => JsonResponse::number(self::numeric($value)),
            'media' => self::media($value),
            'details' => self::details($value),
            'attributes' => self::stringList($value),
            'ippure' => self::ippure($value),
            'connectivity' => self::renderPlain(self::asObject($value), self::CONNECTIVITY_FIELDS),
            'exit' => self::renderPlain(self::asObject($value), self::EXIT_FIELDS),
            'lifecycle' => self::renderPlain(self::asObject($value), self::LIFECYCLE_FIELDS),
            'classification' => self::classification($value),
            'attempts' => self::exitIPAttempts($value),
            'typeEvidenceList' => is_array($value) ? array_map(self::typeEvidence(...), $value) : [],
            'evidenceList' => is_array($value) ? array_map(self::evidence(...), $value) : [],
            'nullableBool' => $value === null ? null : (bool)$value,
            'object', 'raw' => self::rawValue($value),
            default => self::rawValue($value),
        };
    }

    /** media 保留上游原始字节（Go 的 Media.MarshalJSON 直接回吐 raw）。 */
    private static function media(mixed $value): array
    {
        if (!is_array($value)) {
            return [];
        }
        $out = [];
        foreach ($value as $item) {
            $out[] = self::rawValue($item);
        }
        return $out;
    }

    /** details 只保留 k/v 两列（domain.Detail 没有第三个字段）。 */
    private static function details(mixed $value): array
    {
        if (!is_array($value)) {
            return [];
        }
        $out = [];
        foreach ($value as $item) {
            $obj = self::asObject($item);
            $out[] = [
                'k' => property_exists($obj, 'k') ? (string)self::scalarString($obj->k) : '',
                'v' => property_exists($obj, 'v') ? (string)self::scalarString($obj->v) : '',
            ];
        }
        return $out;
    }

    /** ippure.attributes 是 []string。 */
    private static function stringList(mixed $value): array
    {
        if (!is_array($value)) {
            return [];
        }
        $out = [];
        foreach ($value as $item) {
            $out[] = (string)self::scalarString($item);
        }
        return $out;
    }

    /**
     * ippure 载荷。
     *
     * 先复刻 Go 的 `IPPureResult.UnmarshalJSON` 派生（分类 IP 与出口 IP 一致
     * 且带类型证据时重算 state / 重写 attributes），再走形状规范化 ——
     * 顺序不能反：形状位是从**原始** JSON 读出来的，派生只改值不改字段集合。
     */
    private static function ippure(mixed $value): array
    {
        $obj = IpClassification::normalize(self::asObject($value));
        return self::renderShaped($obj, self::IPPURE_FIELDS);
    }

    /** ipcheck.Result：普通结构体语义。 */
    private static function classification(mixed $value): array
    {
        $obj = self::asObject($value);
        $fields = [
            ['version',   false, 'number'],
            ['ip',        false, 'string'],
            ['checkedAt', false, 'string'],
            ['state',     false, 'string'],
            ['types',     true,  'typeEvidenceList'],
            ['evidence',  true,  'evidenceList'],
        ];
        return self::renderPlain($obj, $fields);
    }

    /** ipcheck.TypeEvidence：provider 必出，usageType/companyType 可省。 */
    private static function typeEvidence(mixed $value): array
    {
        return self::renderPlain(self::asObject($value), [
            ['provider',    false, 'string'],
            ['usageType',   true,  'string'],
            ['companyType', true,  'string'],
        ]);
    }

    /** ipcheck.Evidence：hosting 是 *bool，null 视为省略、false 照常输出。 */
    private static function evidence(mixed $value): array
    {
        return self::renderPlain(self::asObject($value), [
            ['queryStatus', true,  'string'],
            ['httpStatus',  true,  'number'],
            ['provider',    false, 'string'],
            ['checkedAt',   false, 'string'],
            ['ip',          false, 'string'],
            ['asn',         true,  'string'],
            ['asnType',     true,  'string'],
            ['companyType', true,  'string'],
            ['usageType',   true,  'string'],
            ['hosting',     true,  'nullableBool'],
            ['vpn',         false, 'bool'],
            ['proxy',       false, 'bool'],
            ['outcome',     false, 'string'],
        ]);
    }

    /** domain.ExitIPAttempt：provider/endpoint/round/elapsedMs 必出，code 可省。 */
    private static function exitIPAttempts(mixed $value): array
    {
        if (!is_array($value)) {
            return [];
        }
        $out = [];
        foreach ($value as $item) {
            $out[] = self::renderPlain(self::asObject($item), [
                ['provider',  false, 'string'],
                ['endpoint',  false, 'string'],
                ['round',     false, 'number'],
                ['elapsedMs', false, 'number'],
                ['code',      true,  'string'],
            ]);
        }
        return $out;
    }

    /**
     * 「空值」判定，对齐 Go 的 jsonEmpty(reflect.Value)：
     * 数组/切片/map/字符串看长度，其余看 IsZero。
     */
    private static function isEmpty(mixed $value): bool
    {
        if ($value === null || $value === false || $value === 0 || $value === 0.0 || $value === '') {
            return true;
        }
        if (is_array($value)) {
            return $value === [];
        }
        if ($value instanceof stdClass) {
            return get_object_vars($value) === [];
        }
        return false;
    }

    /**
     * 各类型的零值。
     *
     * 切片类返回 `[]` 而不是 `null`：走到这里的都是「原载荷有这个键、值为空
     * 容器」的情况（原值为 null 的分支在调用处已经单独处理）。Go 侧此时会
     * 重新 marshal 解码后的值，`[]Media{}` 是非 nil 的空切片 → 输出 `[]`。
     * 早先这里返回 null，导致 `"media": []` 的节点被改写成 `"media": null`，
     * 前端把「明确为空」和「没有这项信息」当成了同一件事。
     */
    private static function zero(string $kind): mixed
    {
        return match ($kind) {
            'bool' => false,
            'number' => 0,
            'media', 'details', 'attributes', 'attempts' => [],
            'string' => '',
            default => null,
        };
    }

    private static function asObject(mixed $value): stdClass
    {
        if ($value instanceof stdClass) {
            return $value;
        }
        if (is_array($value)) {
            // 关联数组按对象处理；空数组表示空对象。
            return (object)$value;
        }
        return new stdClass();
    }

    private static function scalarString(mixed $value): string
    {
        if (is_string($value)) {
            return $value;
        }
        if (is_bool($value)) {
            return $value ? 'true' : 'false';
        }
        if (is_float($value) && $value === floor($value) && abs($value) < 9.0e15) {
            return (string)(int)$value;
        }
        return is_scalar($value) ? (string)$value : '';
    }

    private static function numeric(mixed $value): float|int|string|null
    {
        if (is_int($value) || is_float($value) || is_string($value) || $value === null) {
            return $value;
        }
        return null;
    }

    /**
     * 把 stdClass / 数组转成「保持键序」的输出结构。
     * json_encode 对 PHP 关联数组仍然输出 JSON 对象，顺序即插入顺序，
     * 与 Go 的声明顺序策略一致。
     */
    private static function rawValue(mixed $value): mixed
    {
        if ($value instanceof stdClass) {
            $out = [];
            foreach (get_object_vars($value) as $key => $item) {
                $out[$key] = self::rawValue($item);
            }
            return $out === [] ? new stdClass() : $out;
        }
        if (is_array($value)) {
            // 关联数组要当对象输出，不能按 list 处理 —— 否则 {"status":"active"}
            // 会被序列化成 ["active"]，前端读 status 直接 undefined。
            if (!array_is_list($value)) {
                $out = [];
                foreach ($value as $key => $item) {
                    $out[$key] = self::rawValue($item);
                }
                return $out;
            }
            $out = [];
            foreach ($value as $item) {
                $out[] = self::rawValue($item);
            }
            return $out;
        }
        return $value;
    }
}
