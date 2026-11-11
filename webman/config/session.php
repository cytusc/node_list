<?php
return [
    'type' => 'file',
    'handler' => Workerman\Protocols\Http\Session\FileSessionHandler::class,
    'config' => ['file' => ['save_path' => runtime_path('sessions')]],
    'session_name' => 'node_explorer_session',
    'lifetime' => 28800,
    'cookie_lifetime' => 0,
    'auto_update_timestamp' => false,
    'http_only' => true,
    'same_site' => 'Strict',
    'secure' => filter_var(env('AUTH_COOKIE_SECURE', 'true'), FILTER_VALIDATE_BOOLEAN),
    'cookie_path' => '/',
];
