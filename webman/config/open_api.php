<?php
declare(strict_types=1);

// 对外接口使用独立的服务端密钥；未配置时关闭，不接受网页登录会话。
return [
    'app_id' => (string)env('OPEN_API_APP_ID', ''),
    'secret' => (string)env('OPEN_API_SECRET', ''),
];
