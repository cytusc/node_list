<?php
declare(strict_types=1);

namespace app\controller;

use app\service\JsonResponse;
use support\Request;
use support\Response;

/**
 * /api/downloads 与 /api/download —— 订阅下载占位接口。
 *
 * 这两个路径是显式的 404 占位（"订阅下载暂不可用。"），语义是「功能未开」，
 * 与「路径不存在」（接口不存在。）要区分开：前端可以据此提示「下载能力未上线」
 * 而不是「地址写错了」。
 * 因此必须保留这两个路由，不能交给 /api/* 兜底。
 */
final class DownloadController
{
    public function unavailable(Request $request): Response
    {
        return JsonResponse::error(404, '订阅下载暂不可用。');
    }
}
