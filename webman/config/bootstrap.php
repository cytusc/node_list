<?php
/**
 * 启动阶段执行的 Bootstrap 类列表（每个类需实现 Webman\Bootstrap::start($worker)）。
 *
 * 目前为空：数据库连接池由 webman/database 在首次使用时惰性建立，
 * 路由/中间件/配置加载都在 support/bootstrap.php 里完成，没有需要预热的东西。
 *
 * 之所以保留这个空文件：config('bootstrap') 在文件缺失时虽然会退回默认空数组，
 * 但「去哪里加启动钩子」需要有个明确答案，否则下一个人会往 start.php 里加代码。
 */

return [];
