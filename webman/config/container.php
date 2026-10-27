<?php
/**
 * 容器。
 *
 * 用 webman 自带实现而不是 PHP-DI：框架在 `App::getCallback()` 里对控制器调用
 * `$container->make($class)`，而 PHP-DI 只实现 PSR-11 的 get()/has()、没有 make()，
 * 接上去会在控制器实例化时直接抛「undefined method」。
 *
 * 本服务需要的两个容器能力，webman 内置实现都满足：
 *   1. `get()` 进程内单例 —— 服务类无状态，构造一次复用
 *   2. `make()` 带构造参数实例化 —— 控制器由框架按需创建
 *
 * 依赖注入方向用 webman 原生的「控制器方法参数注入」：
 *   public function index(Request $request, LiveResults $live) { ... }
 * 框架会用 get() 解析参数类型，无需构造函数注入，也就不需要反射自动装配。
 */

return new Webman\Container();
