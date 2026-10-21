# OpenCode 代理 Unicode 配置修复

OpenCode 的运维脚本独立部署在 `/root/docker/opencode/scripts`，不包含在 node_list 镜像中。`unicode-config.patch` 保存此次线上脚本修复及其回归测试，不包含代理配置或密钥。

Python 默认的 JSON 序列化会把旗帜等非 BMP 字符写成 UTF-16 代理对转义。Mihomo v1.19.27 使用 YAML 解析器读取该 JSON，因而报 `yaml: found invalid Unicode character escape code` 并反复重启。补丁使磁盘配置和控制器请求中解码后的配置都保留 Unicode 字符，同时保留文件 inode，兼容单文件 Docker bind mount。

在保存脚本备份并暂停可能写配置的 OpenCode 定时任务后，于 OpenCode 部署目录执行：

```sh
patch --dry-run -p1 < /path/to/unicode-config.patch
patch -p1 < /path/to/unicode-config.patch
cd scripts
python3 -m unittest test_maintain_pool.PoolTests.test_reload_routes_preserves_unicode_in_file_and_controller_payload
```

已应用补丁的部署可使用 `patch --dry-run -R -p1` 检查，无需重复应用。

已有代理配置还需要先用 `json.loads` 读取，再以 `json.dumps(config, ensure_ascii=False)` 和 UTF-8 编码保存候选文件。候选文件必须通过同版本 Mihomo 的 `-t -f` 校验后才能替换线上配置。原子替换单文件 bind mount 对应的宿主文件后，需要重建 `opencode-proxy` 容器以挂载新 inode。恢复定时任务后检查代理稳定性、两个 API 的 `/healthz` 和真实模型请求。

此次线上验收中，两实例健康接口均恢复 HTTP 200，`minimax-m3.1-flash-free` 均成功返回内容；GLM、DeepSeek 和 Big Pickle 仍返回上游 HTTP 429，不能仅凭健康检查判定所有模型可用。原有 OpenCode 全量测试存在 2 项失败和 2 项错误，已在修改前版本复现；上述新增 Unicode 回归测试通过。
