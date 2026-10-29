<?php
/**
 * PostgreSQL 连接。启用对外接口时，还需 nonce 表的 SELECT/INSERT/UPDATE/DELETE 权限。
 *
 * 环境变量名与 compose 的 x-environment 完全同名：
 *   PGHOST / PGPORT / PGDATABASE / PGUSER / PGPASSWORD / PGPOOL_MAX
 */

return [
    'default' => 'pgsql',
    'connections' => [
        'pgsql' => [
            'driver' => 'pgsql',
            'host' => env('PGHOST', '127.0.0.1'),
            'port' => (int)env('PGPORT', 5432),
            'database' => env('PGDATABASE', 'node_explorer'),
            'username' => env('PGUSER', ''),
            'password' => env('PGPASSWORD', ''),
            'charset' => 'utf8',
            'prefix' => '',
            'schema' => env('PGSCHEMA', 'public'),
            'sslmode' => env('PGSSLMODE', 'prefer'),
            // 服务端预处理：既避免重复解析 SQL，也让 pg 侧能复用执行计划。
            'options' => [
                PDO::ATTR_EMULATE_PREPARES => false,
                PDO::ATTR_TIMEOUT => 10,
            ],
            'pool' => [
                'max_connections' => (int)env('PGPOOL_MAX', 8),
                'min_connections' => 1,
                'wait_timeout' => 3,
                'idle_timeout' => 60,
                'heartbeat_interval' => 50,
            ],
        ],
    ],
];
