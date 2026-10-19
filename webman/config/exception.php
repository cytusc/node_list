<?php
/**
 * 异常处理器映射。
 *
 * app\service\ApiError 走专用处理器（回 {error, code}），其余异常走默认处理器
 * （已被 app\exception\ApiErrorHandler 覆盖为 JSON 出口）。
 */

return [
    app\service\ApiError::class => app\exception\ApiErrorHandler::class,
    '' => app\exception\ApiErrorHandler::class,
];
