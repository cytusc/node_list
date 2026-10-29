# PHP 对外接口 v1

接口由现有 PHP / webman 服务提供，仅支持服务端通过 HTTPS 调用。使用独立 App ID / Secret 验签，不使用网页登录会话。当前只有国家列表与标准模式节点列表两个接口。

## 启用

先执行数据库迁移（首次建库执行共享 `schema.sql` 已包含此表）：

```bash
psql -v ON_ERROR_STOP=1 -f deploy/sql/023-open-api.sql
```

在运行 PHP 的环境中配置：

```dotenv
OPEN_API_APP_ID=your-service
OPEN_API_SECRET=替换为随机生成的密钥
```

App ID 为 1–64 位英文字母、数字、`_` 或 `-`；Secret 至少 32 字节，建议用 `php -r 'echo bin2hex(random_bytes(32)), PHP_EOL;'` 生成 64 位十六进制文本。这个文本本身就是 HMAC 密钥，不再进行十六进制解码。未配置或配置无效时返回 `503 OPEN_API_DISABLED`。

本地运行配置 `webman/.env`；Compose 部署配置仓库根目录 `.env`，只向 `node-explorer` 注入此密钥，重新创建 API 容器生效。调用方同样只在服务端保存 Secret。

数据库账号除现有节点视图读取权限外，需要 `node_explorer_open_api_nonces` 表的 `SELECT / INSERT / UPDATE / DELETE` 权限。迁移由具备建表权限的账号执行。防重放记录由 PostgreSQL 共享，多 worker、多实例及 API 重启后都有效，无需 Redis。

## 验签

每次请求携带以下请求头：

| 请求头 | 格式 |
| --- | --- |
| `X-App-Id` | 配置的 App ID |
| `X-Timestamp` | 当前 Unix 秒时间戳，10 位十进制整数，允许前后偏差 300 秒 |
| `X-Nonce` | 每次请求重新生成，16–128 位字母、数字、`_` 或 `-`；建议 16 个随机字节的 32 位十六进制文本 |
| `X-Signature` | HMAC-SHA256 的 64 位小写十六进制文本 |

待签名字符串由以下六行用单个 LF（`\n`）拼接，末尾没有换行：

```text
GET
/api/open/v1/nodes
country=HK&mode=standard&page=1
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
<X-Timestamp 原文>
<X-Nonce 原文>
```

规则：

1. HTTP 方法大写；路径为请求路径，包含 `/api/open/v1`，不包含域名和查询字符串。反代应保留此路径。
2. 查询参数先 URL 解码，再按参数名的字节序升序排列，并将每个名称和值按 RFC 3986 编码，用 `=`、`&` 拼接；空格编码为 `%20`，不能用 `+`。入站 URL 中的 `+` 解码为空格后统一编码。
3. 签入实际发送的全部查询参数。不补入省略的默认值，不提前将 `hk` 转换成 `HK`；这些业务处理发生在验签之后。无查询参数时第三行为空行。
4. 第四行为原始请求体的 SHA256 小写十六进制值。这两个 GET 接口要求无请求体，因此固定为上述空字符串摘要。
5. `signature = hash_hmac('sha256', string_to_sign, secret)`，使用常量时间比较。

查询字符串最多 4096 字节、16 个参数；拒绝重复名称（包括解码后重复）、数组、带点或空格的键名、错误百分号编码和无效 UTF-8。业务层只接受各接口列出的参数，不接受 `limit`、`page_size` 或自定义排序。

验签成功后原子登记 nonce，保留 601 秒，覆盖整个时间戳接受窗口（含边界）。相同 App ID 在此期间不能重复使用 nonce，即使更换路径、页码或时间戳。重试请求必须重新生成 nonce 并重新签名。过期记录随请求每个 worker 最多每分钟清理一次；nonce 存储不可用时拒绝请求。

跨语言校验向量：Secret 为文本 `0123456789abcdef0123456789abcdef`，方法 `GET`，路径 `/api/open/v1/nodes`，查询参数 `country=HK&mode=standard&page=1`，空请求体，时间戳 `1700000000`，nonce `0123456789abcdef0123456789abcdef`。预期签名：

```text
25e32218e7a5fc863eee92cbd06b91c3745503ed9d550ca15e01b7c9404b5d1c
```

该向量用于验证签名算法，时间戳已过期，不能直接请求接口。

## 国家列表

```http
GET /api/open/v1/countries
```

不接受查询参数。返回当前有可对外提供节点的国家/地区，按代码升序：

```json
{
  "code": "OK",
  "data": [
    {"code": "HK", "name": "中国香港特别行政区", "node_count": 18}
  ]
}
```

没有节点时 `data` 为 `[]`。国家名称复用项目已有地区字典，代码为大写两位地区代码。无归属地、`ZZ` 及无有效分享链接的节点不计入。

## 节点列表

```http
GET /api/open/v1/nodes?country=HK&mode=standard&page=1
```

| 参数 | 规则 |
| --- | --- |
| `country` | 必填，使用国家列表返回的两位代码，也接受小写 |
| `mode` | 可省略，默认 `standard`，当前仅支持此值 |
| `page` | 可省略，默认 `1`，范围 1–2147483647，不接受前导零 |

每页固定 **10 条**。下面是节点字段示例（示意最后一页只有一个节点）：

```json
{
  "code": "OK",
  "data": {
    "country": "HK",
    "mode": "standard",
    "page": 1,
    "page_size": 10,
    "total": 1,
    "has_more": false,
    "nodes": [
      {
        "id": "node-identity",
        "name": "香港节点",
        "type": "vless",
        "country": "HK",
        "exit_ip": "203.0.113.10",
        "link": "vless://example@example.com:443",
        "network_type": "住宅IP",
        "residential": true,
        "risk_score": 5,
        "human_percent": 80,
        "bot_percent": 20,
        "human_preferred": true,
        "native": true
      }
    ]
  }
}
```

`link` 优先使用保存的非空原始分享链接，逐字保留；缺失时使用已保存的生成链接。两者都没有的节点不对外提供。只返回表中示例的公开契约字段，不输出原始配置、来源记录或供应商证据。`link` 包含连接凭据，成功和失败响应均设置 `Cache-Control: no-store`。

`residential`、`human_preferred`、`native` 均为 `true / false / null`，`null` 表示未知。风险分数和比例为 0–100 的数值，缺失、无效数值保持 `null`，其中 `0` 是有效值。

### 标准模式

先筛选指定国家，再按下列优先级逐项比较，前项相同才比较下一项，最后在数据库中分页：

1. 已确认住宅 → 已确认非住宅 → 类型未知。
2. 风险度数值升序，缺失排在当前类型组末尾。
3. 人类比例严格大于机器比例 → 小于或等于 → 任一比例缺失。此项不额外按人类比例数值排序。
4. 原生 IP → 广播 IP → 未知。
5. 节点 ID 按字节序升序，保证同条件下顺序确定。

住宅分类与现有 PHP 类型归一逻辑一致：每个供应商首条类型证据一票、`usageType` 优先于 `companyType`，最多票平局沿用现有住宅、商业、机房规则；分类版本必须匹配当前 `IPCHECK_VOTING_VERSION`。不使用节点名称或未验证的住宅标签提升排序。

属性必须对应当前出口，分类本身的 IP 也必须匹配；保留的历史属性和 `stale` fallback 均不参与排序和属性输出。风险分数与原生判定需要属性查询成功。人类/机器比例是 ASN 统计，并非该节点实际流量统计。IPv6 或属性查询未完成的节点仍可提供连接，对应属性保持未知。

国家统计和节点查询使用同一套资格条件：现有实时发布视图要求 `available=true`、当前配置与实例的测活成功且在 6 小时内、出口已验证且在 6 小时内；对外接口还要求有效地区代码和非空链接。归属地沿用项目视图中的已固定国家策略。不会因缺失风险评分或缺失住宅分类而排除可用节点。

有效格式的国家当前没有节点时返回 `total: 0`、`nodes: []`；超出范围的页码返回空数组并保留请求页码。单次节点请求的总数和当页数据在同一个一致性只读事务中读取。国家请求与后续节点请求、不同页请求之间均是实时数据，节点变化可能造成数量变化或跨页重复、遗漏，不提供快照分页。

## 错误

失败响应沿用项目统一结构：`{"code":"INVALID_SIGNATURE","error":"接口验签失败。"}`。

| HTTP | code | 含义 |
| --- | --- | --- |
| 400 | `INVALID_QUERY` | 参数格式错误、重复键、缺少国家或试图修改分页大小 |
| 400 | `INVALID_MODE` | 不支持的模式 |
| 400 | `INVALID_REQUEST` | GET 携带请求体等无效请求 |
| 401 | `INVALID_SIGNATURE` | 请求头缺失、App ID 不符或签名错误 |
| 401 | `REQUEST_EXPIRED` | 时间偏差超过 300 秒 |
| 409 | `REPLAY_DETECTED` | nonce 已使用 |
| 503 | `OPEN_API_DISABLED` | 未配置独立接口密钥 |
| 503 | `AUTH_UNAVAILABLE` | 防重放存储不可用，检查迁移与写权限 |
| 503 | `READ_FAILED` | 节点查询暂不可用 |

## PHP 调用示例

```php
<?php
$origin = rtrim((string)getenv('NODE_API_ORIGIN'), '/'); // https://your-domain.example
$appId = (string)getenv('OPEN_API_APP_ID');
$secret = (string)getenv('OPEN_API_SECRET');

function fetchNodesApi(string $path, array $params = []): array
{
    global $origin, $appId, $secret;
    ksort($params, SORT_STRING);
    $query = http_build_query($params, '', '&', PHP_QUERY_RFC3986);
    $timestamp = (string)time();
    $nonce = bin2hex(random_bytes(16));
    $message = implode("\n", ['GET', $path, $query, hash('sha256', ''), $timestamp, $nonce]);
    $signature = hash_hmac('sha256', $message, $secret);
    $curl = curl_init($origin . $path . ($query === '' ? '' : '?' . $query));
    curl_setopt_array($curl, [
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_CONNECTTIMEOUT => 5,
        CURLOPT_TIMEOUT => 15,
        CURLOPT_HTTPHEADER => [
            'X-App-Id: ' . $appId,
            'X-Timestamp: ' . $timestamp,
            'X-Nonce: ' . $nonce,
            'X-Signature: ' . $signature,
        ],
    ]);
    $body = curl_exec($curl);
    $status = curl_getinfo($curl, CURLINFO_HTTP_CODE);
    if ($body === false || $status !== 200) {
        throw new RuntimeException('Node API request failed: HTTP ' . $status);
    }
    return json_decode($body, true, 512, JSON_THROW_ON_ERROR)['data'];
}

$countries = fetchNodesApi('/api/open/v1/countries');
if ($countries !== []) {
    $page = fetchNodesApi('/api/open/v1/nodes', [
        'country' => $countries[0]['code'],
        'mode' => 'standard',
        'page' => '1',
    ]);
}
```

## 验证

```bash
npm test
PGHOST=127.0.0.1 PGDATABASE=node_explorer_test PGUSER=postgres PGPASSWORD=test_password \
  php webman/tests/open-api-integration.php
```

离线测试覆盖签名向量、参数歧义、请求篡改、时间戳、过期 nonce 回收和失败关闭。集成测试在名称包含 `test` 的专用 PostgreSQL 数据库中创建随机 schema 并清理，验证完整 SQL 排序、分类归一一致性、分页、可用条件、属性缺失及真实防重放存储。已加入 npm、Composer 测试入口和 CI。
