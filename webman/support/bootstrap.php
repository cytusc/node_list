<?php
/**
 * 应用启动脚本（webman 骨架约定的路径，由 support\App::run() 在 worker 启动时 require）。
 *
 * 骨架里这份文件是框架自带引导脚本的副本，内容为：
 *   .env 载入 → Config::load(config/, 排除 route) → 时区
 *   → autoload.files → middleware → config/bootstrap.php 里的 Bootstrap 类 → Route::load()
 *
 * 这里改成直接 require 框架内的同一份实现，理由：
 *   1. 两份完全相同的启动顺序代码，改一份忘另一份就是「开发能跑、生产 500」的经典来源；
 *   2. 本项目没有插件（plugin/）也没有自定义 Bootstrap 类，骨架副本里的那些分支全是空转。
 *
 * 如果以后需要在启动阶段做应用专属动作（预热连接池、注册信号等），
 * 不要改这个文件，而是在 config/bootstrap.php 里加一个 Bootstrap 实现类 ——
 * 那份配置会被下面的脚本读取，语义比往这里堆代码清晰。
 */

require_once __DIR__ . '/../vendor/workerman/webman-framework/src/support/bootstrap.php';
