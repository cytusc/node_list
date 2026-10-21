<?php
declare(strict_types=1);

namespace app\controller;

use app\service\LiveResults;
use app\service\ResultsQueryParser;
use app\service\ResultsStatus;
use support\Request;
use support\Response;
use app\service\ApiError;
use app\service\JsonResponse;

/**
 * GET /api/results —— 当前节点实时视图。
 *
 * 依赖通过方法参数注入（webman 原生的「控制器参数绑定」）：
 * 容器会把 LiveResults / ResultsStatus 作为进程内单例复用，
 * 不需要在控制器里 new，也不需要构造函数注入。
 *
 * 与 Go 的 api.liveResults 一致的行为要点：
 *   1. 带 snapshot 参数 → 400「仅支持查询当前节点状态。」
 *   2. 分页/筛选/排序参数不合法 → 400，文案固定
 *   3. 无 page/limit → 不分页，返回全量节点（page/limit 回 0）
 *   4. 响应固定附带 ippureCollector 与 sourceWarning（后者 null 表示上游读取正常）
 *   5. Cache-Control: no-store（前端 5s 轮询，不能被中间层缓存）
 */
final class ResultsController
{
    public function index(Request $request, LiveResults $live, ResultsStatus $status): Response
    {
        $params = $request->get();

        // 实时视图没有历史批次概念。注意 view=aggregate 在 Go 的 live 分支里是被
        // 忽略的，这里同样不校验，避免出现「Go 能跑、PHP 报 400」的差异。
        if (array_key_exists('snapshot', $params)) {
            throw ApiError::invalidQuery('仅支持查询当前节点状态。');
        }

        if (!config('api.live', true)) {
            // 关掉读路径时明确报 503，而不是静默返回空列表 —— 前者能被监控发现，
            // 后者会让前端显示「0 个节点」这种看似正常的假象。
            throw ApiError::readFailed();
        }

        // 参数校验在解析阶段完成，非法输入在这里就 400，不会进入 SQL 层。
        $query = ResultsQueryParser::fromQuery($params);

        $body = $live->query($query);
        // 附属信息最后合并：即使采集器状态读取失败，也不影响结果本身。
        //
        // 顺序照 Go 的 liveResults：它写的是
        //   struct{ postgres.Result; Collector `ippureCollector`; SourceWarning `sourceWarning` }
        // 嵌入字段会被摊平在原位，所以 ippureCollector 在前、sourceWarning 在后。
        // （非 live 的 resultsResponse 顺序相反，别照那个抄——那条路径在生产不可达。）
        $body['ippureCollector'] = $status->collectorStatus();
        $body['sourceWarning'] = $status->sourceWarning();

        return JsonResponse::payload($body);
    }
}
