<?php
declare(strict_types=1);
namespace app\controller;

use app\service\{ApiError, JsonResponse};
use support\{Db, Log, Request, Response};
use Throwable;

final class NodeLinkController
{
    public function index(Request $request): Response
    {
        if ($request->header('x-node-explorer') !== '1') return JsonResponse::error(403, '请求来源无效。', '', true);
        $id = $request->post('identity');
        if (!is_string($id) || $id === '' || strlen($id) > 512) return JsonResponse::error(400, '节点标识无效。', '', true);
        try {
            // Same active/retired/source membership rules as the displayed list.
            $row = Db::table('node_explorer_live_facts')->where('identity', $id)->selectRaw("node->>'rawLink' AS link")->first();
        } catch (Throwable $e) {
            Log::warning('node link lookup failed');
            throw ApiError::readFailed();
        }
        if (!$row) return JsonResponse::error(404, '节点已失效，请刷新列表。', '', true);
        $link = $row->link ?? '';
        if (is_string($link) && trim($link) !== '') return JsonResponse::payload(['format' => 'link', 'content' => $link]);
        return JsonResponse::error(422, '此节点未保存原始链接。', '', true);
    }
}
