#!/usr/bin/env python3
"""从 Go 的 domain/countries.go 生成 PHP 的 Country 映射类。

背景：地区中文名原本由 Node 的 Intl.DisplayNames(zh-CN) 生成，Go 侧已固化成
generated 文件（backend/internal/domain/countries.go）。PHP 版不能再依赖运行期
ICU 数据（容器里未必装 intl 扩展，且版本差异会让标签漂移），所以同样固化成常量表，
并由本脚本保证两边一致。

用法（仓库根目录）：
    python webman/tools/gen-country-names.py

输出：webman/app/service/Country.php（除模板外的 NAMES 常量由脚本重写）
"""

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SRC = ROOT / "backend/internal/domain/countries.go"
OUT = ROOT / "webman/app/service/Country.php"

TEMPLATE = '''<?php
declare(strict_types=1);

/**
 * ISO 3166-1 alpha-2 → 简体中文显示名。
 *
 * 由 webman/tools/gen-country-names.py 从 backend/internal/domain/countries.go
 * 逐项生成（原文件源自 Intl.DisplayNames(zh-CN) 区域数据）。**不要手工编辑**：
 * 两边必须逐字一致，否则同一节点在 Go/PHP 两套接口下会显示不同地区名。
 *
 * 不依赖 intl 扩展：容器里可能没装，且 ICU 版本升级会让标签漂移，
 * 地区名用于展示与排序，必须保持稳定。
 */

namespace app\\service;

final class Country
{
    /** 未知地区的统一显示名（Go 的 CountryName("") 与 CountryName("unknown") 都返回它）。 */
    public const UNKNOWN_LABEL = '未知地区';

    /** @var array<string, string> */
    private const NAMES = [
%s
    ];

    /**
     * 解析地区代码的显示名。
     *
     * 与 Go 的 domain.CountryName 完全一致：空值与 "unknown" 归到「未知地区」，
     * 未收录的新代码原样返回，避免新地区被折叠成空白。
     */
    public static function name(string $code): string
    {
        if ($code === '' || $code === 'unknown') {
            return self::UNKNOWN_LABEL;
        }
        return self::NAMES[$code] ?? $code;
    }

    /**
     * 返回中文名包含关键词的 ISO 地区代码，顺序为 A..Z 字典序。
     *
     * 对应 Go 侧 live.go 的搜索前置展开：SQL 里只做 region = ANY(...)，
     * 所以把「用户搜索的中文地区名」先翻译成代码集合，语义才与原实现一致。
     *
     * @return list<string>
     */
    public static function codesMatching(string $needle): array
    {
        if ($needle === '') {
            return [];
        }
        $needle = mb_strtolower($needle, 'UTF-8');
        $codes = [];
        // 用 range 而不是 $a++：PHP 的字符串自增在 'Z' 之后变成 'AA'，
        // 按字典序仍小于 'Z'，会变成死循环（Go 侧是 rune 自增，'Z'++ 即出界）。
        foreach (range('A', 'Z') as $a) {
            foreach (range('A', 'Z') as $b) {
                $code = $a.$b;
                if (str_contains(mb_strtolower(self::name($code), 'UTF-8'), $needle)) {
                    $codes[] = $code;
                }
            }
        }
        return $codes;
    }

    /** 收录的地区代码数量，供测试断言使用（防止生成结果被截断）。 */
    public static function count(): int
    {
        return count(self::NAMES);
    }
}
'''


def main() -> int:
    if not SRC.is_file():
        print(f"source not found: {SRC}", file=sys.stderr)
        return 1
    text = SRC.read_text(encoding="utf-8")
    pairs = re.findall(r'^\t"([^"]+)":\s+"([^"]+)",$', text, re.M)
    if not pairs:
        print("no entries parsed from countries.go", file=sys.stderr)
        return 1
    rows = "\n".join("\t\t'%s' => '%s'," % (code, name) for code, name in pairs)
    OUT.write_text(TEMPLATE % rows, encoding="utf-8", newline="\n")
    print(f"wrote {OUT} with {len(pairs)} entries")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
