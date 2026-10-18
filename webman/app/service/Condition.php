<?php
declare(strict_types=1);

namespace app\service;

/**
 * 一个 SQL 条件片段及其绑定参数。
 *
 * 为什么把「片段 + 参数」绑成一个值对象，而不是像 Go 版那样用一个
 * `&$args` 引用边拼边塞：
 *   - 绑定顺序由数据结构的顺序决定，不依赖函数调用顺序的副作用，改代码不容易错位
 *   - 每个条件自带参数，可以单独单测（断言 SQL 文本与参数列表）
 *   - 组合时用 `and()`，最终一次性 flatten，不会出现「参数 push 了但 SQL 没拼上」
 *
 * 所有值都是绑定参数；片段里出现的标识符只来自枚举与常量。
 */
final class Condition
{
    /**
     * @param list<mixed> $bindings
     */
    public function __construct(
        public readonly string $sql,
        public readonly array $bindings = [],
    ) {
    }

    /** 不带参数的片段（列名/常量比较）。 */
    public static function raw(string $sql): self
    {
        return new self($sql);
    }

    /**
     * `column = ANY(ARRAY[?,?,...])`。
     *
     * @param list<string> $values 非空
     */
    public static function inList(string $column, array $values): self
    {
        $placeholders = implode(',', array_fill(0, count($values), '?'));
        return new self(
            $column . ' = ANY(ARRAY[' . $placeholders . ']::text[])',
            array_values($values)
        );
    }

    /** 两个片段用 AND 连接，参数按左→右顺序保留。 */
    public function and(self $other): self
    {
        return new self(
            '(' . $this->sql . ') AND (' . $other->sql . ')',
            [...$this->bindings, ...$other->bindings]
        );
    }

    /**
     * 把一组条件合并成 WHERE 片段与参数列表。
     *
     * @param list<self> $conditions
     * @return array{0:string, 1:list<mixed>} [where 子句（含前导空格），绑定参数]
     */
    public static function merge(array $conditions): array
    {
        if ($conditions === []) {
            return ['', []];
        }
        $sql = [];
        $bindings = [];
        foreach ($conditions as $condition) {
            $sql[] = $condition->sql;
            foreach ($condition->bindings as $binding) {
                $bindings[] = $binding;
            }
        }
        return [' WHERE ' . implode(' AND ', $sql), $bindings];
    }
}
