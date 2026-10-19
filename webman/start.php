#!/usr/bin/env php
<?php
/**
 * webman 启动入口（node_list 网页接口的唯一实现）。
 *
 * 启动顺序：读 .env → 加载 config/*.php → support\App::run() 拉起 Workerman。
 * 骨架中这份文件只做三件事，不要在这里加业务逻辑：
 *   - chdir 保证相对路径（runtime/、storage/）落在 webman/ 下；
 *   - require vendor/autoload.php（含 app/functions.php）；
 *   - support\App::run() 内部会 require support/bootstrap.php 完成框架引导。
 */
chdir(__DIR__);
require_once __DIR__ . '/vendor/autoload.php';

support\App::run();
