<?php
declare(strict_types=1);

namespace app\model;

use Illuminate\Database\Eloquent\Model;

/**
 * 服务心跳（只读模型）。
 *
 * 表：node_explorer_service_status(service text primary key, updated_at timestamptz, payload jsonb)
 *
 * 说明：本项目的**热路径**（结果分页）是一条手写 SQL —— 元数据、facets、过滤、
 * 排序、分页必须在同一个数据库快照里一次算完，交给 ORM 会被拆成多次往返并丢失
 * 「一条语句一个快照」的一致性。ORM 只用在**单表按主键取值**这类简单读上，
 * 这里就是典型场景：取 source-ingest 心跳里的 sourceWarning。
 *
 * 刻意写成只读用途（不提供写方法、不开放 fillable）：本服务是纯读接口，
 * 数据库写路径仍由 Go 侧的采集/调度服务负责，两边不会争抢写入语义。
 */
class ServiceStatus extends Model
{
    protected $table = 'node_explorer_service_status';

    protected $primaryKey = 'service';

    /** 主键是字符串服务名，不是自增 id。 */
    public $incrementing = false;

    protected $keyType = 'string';

    /** 表里没有 created_at/updated_at 这对 Laravel 约定列（updated_at 语义不同，由服务自己写）。 */
    public $timestamps = false;

    /** jsonb 列自动解码为数组，业务侧直接 $model->payload['sourceWarning']。 */
    protected $casts = [
        'payload' => 'json',
        'updated_at' => 'datetime',
    ];

    /** 只读模型：显式禁掉批量赋值白名单的漏洞面。 */
    protected $guarded = ['*'];
}
