<?php
/**
 * 全局中间件。
 *
 * 仅一个访问日志中间件；鉴权/限流本服务不需要（接口是公开只读的看板数据，
 * 由反代层做 IP 白名单与限流）。
 */

return [
    '' => [
        app\middleware\AccessLog::class,
    ],
];
