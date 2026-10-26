<?php
/**
 * 全局中间件。
 *
 * 全局访问日志；节点与监控路由另挂 Authenticate，登录端点限制尝试次数。
 */

return [
    '' => [
        app\middleware\AccessLog::class,
    ],
];
