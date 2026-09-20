# Nakama PlayFlow FleetManager

Nakama 社区版的 PlayFlow FleetManager 适配器，配套 Unity Server Agent。面向 **一个 Unity Linux/FishNet 进程承载多个双人房间** 的场景。属于社区项目，非 Heroic Labs、PlayFlow 或 Edgegap 官方插件。

当前是 `0.1.0-dev`：可构建、可加载、可本地验证的集成基础。真实游戏适配、PlayFlow 云端验收、容量压测和业务结算服务仍需完成，不能把本地模拟结果视为生产验收。

## 两个仓库

| 仓库 | 职责与分发方式 |
| --- | --- |
| [nakama-playflow](https://github.com/xuhuanhello/nakama-playflow)（本仓库） | Go 库、Nakama `.so`、官方镜像派生构建、数据库迁移、测试与部署说明 |
| [playflow-server-nakama-plugin-unity](https://github.com/xuhuanhello/playflow-server-nakama-plugin-unity) | 根目录 UPM 包；Server Agent、客户端分配查询、凭证验证、Host 接口与样例 |

发布方式与 Edgegap 的 Go + Unity 配套模式类似。当前通过两个仓库的 `main` 分支提供源码，尚无稳定版本标签。Go 模块路径为 `github.com/xuhuanhello/nakama-playflow`；编译 Nakama 插件时仍须遵守下面的版本基线。

```sh
git clone --branch main https://github.com/xuhuanhello/nakama-playflow.git
cd nakama-playflow
```

已有 Go runtime 工程可使用 `go get github.com/xuhuanhello/nakama-playflow/pkg/fleetmanager@main` 引入模块。Unity Package Manager 选择 **Add package from git URL**，填入：

```text
https://github.com/xuhuanhello/playflow-server-nakama-plugin-unity.git#main
```

`main` 会继续变化；用于可重复部署时，请记录两个仓库各自验证过的 commit SHA。

## 已实现

- 实现并注册官方 `runtime.FleetManagerInitializer`，包含 `Create/Join/Get/List/Update/Delete`，无需修改 Nakama 源码。
- PlayFlow v3 Start/Get/List/Stop、真实分页、端口映射、超时、创建结果不确定时的认领与重试保护。
- 双人 Matchmaker hook → 房间准备 → 原子预留两席 → 各自查询签名凭证；同一进程可承载多个房间。
- PostgreSQL 中持久化实例、分配、席位、命令、通知修订号和控制器租约；取消清理确认前保持容量占用。
- 认证 bootstrap、心跳序列、启动代次、命令去重及执行重试、失联隔离、drain 与停止确认。
- 按可用房间和待分配需求扩容；闲置缩容等待房间、玩家、模拟、复核和待提交结果全部清空。
- 官方镜像真实加载检查、Go race 测试、真实 PostgreSQL 测试、真实 Nakama WebSocket 匹配及重启恢复烟雾测试。

Unity/FishNet 的接入边界与尚未完成的生产能力见 [架构与实施范围](docs/architecture.md) 和 [游戏接入清单](docs/game-integration.md)。

本轮实际执行的检查和限制记录在 [验证报告](docs/validation-2026-09-21.md)；完整请求格式见 [协议 v1](docs/protocol-v1.md)。

## 版本基线

| 项目 | 当前测试目标 |
| --- | --- |
| Nakama | `registry.heroiclabs.com/heroiclabs/nakama:3.41.0` |
| pluginbuilder | 同仓库的 `nakama-pluginbuilder:3.41.0` |
| Go / nakama-common | `1.27.1` / `v1.48.0` |
| `.so` 平台 | `linux/amd64` |
| Unity / 协议 | Unity `2022.3+`，协议 `schema_version=1` |

**Go `.so` 与 Nakama 的实际 Go 工具链、共享依赖、构建选项和 CPU 架构绑定。** 使用本仓库固定的官方 builder 构建，升级 Nakama 时重新构建并实际加载测试。Unity 包通过协议通信，不受这个 Go ABI 绑定。详见 [兼容与发布说明](docs/compatibility.md)。

## 本地启动与验证

需要 Docker Desktop（Compose/Buildx）、Go、Node.js 22+、make 和 curl。Apple Silicon 上的 amd64 容器用于功能验证。

```sh
make test test-race
make integration
```

`make integration` 会构建并启动独立的 `pf-nakama-local` Compose 项目，迁移两套数据库，验证插件加载，执行真实 PostgreSQL 测试，然后：

1. 通过官方认证 API 和 WebSocket Matchmaker 建立双人匹配。
2. 模拟 Unity Agent，验证三间房共用两个实例及各席位签名。
3. 验证取消必须收到清理确认。
4. 只重启本测试项目的 Nakama，验证持久化与重新签发重连凭证。
5. 验证 drain 不打断活跃房间，待提交结果清零后才停止实例。

测试不会访问真实 PlayFlow。Mock 不启动游戏进程、不持有 Docker socket；烟雾测试的重启操作由主机上的脚本执行。测试日志不输出凭证。

本地 Nakama API 为 `http://127.0.0.1:17350`，Console 为 `http://127.0.0.1:17351`。开发凭证与明确的重置命令在 [本地部署说明](docs/compatibility.md#running-locally)。测试失败时保存现场，未关闭的模拟房间需要先清理；重跑不会擅自删除数据库。

```sh
make local-logs
make local-down       # 保留数据库卷
PLUGIN_VERSION=0.1.0-dev make package
```

## 使用官方 Nakama 镜像

构建生成 `playflow.so` 后，将它只读挂载到对应官方镜像的 `/nakama/data/modules/playflow.so`。先运行本插件的 `fleet-migrate`，配置独立 fleet 数据库及 [环境变量模板](deploy/production.env.example)，然后启动 Nakama。也可使用本仓库 Dockerfile 的 `runtime` target，它以同一官方镜像为基础加入插件。

已有 Go runtime 工程可在其 `InitModule` 内调用：

```go
return fleetmanager.RegisterFromEnv(ctx, logger, db, nk, initializer)
```

该入口注册默认 FleetManager 和本项目的双人匹配 hook；现有 hook 需要由同一 runtime 工程组合。不能同时加载两个都接管默认 FleetManager/Matchmaker hook 的独立插件。自定义组合可使用 `NewFromEnv`，并按 `Register` 中的接线方式注册自己的 hook 与生命周期。

TypeScript/Lua 可继续处理其他 Nakama 游戏业务，但本项目的 FleetManager 适配层由 Go 实现。单改 TS/Lua 文件不能安装这个 Go 接口实现。

## 生产配置与运行边界

一个 `FLEET_DEPLOYMENT_ID` 对应一个固定的 build、region、PlayFlow 构建版本及容量配置。首次启动会保存配置指纹；改变这些值或签名主密钥必须使用新 namespace，避免新旧游戏进程被混用。初版不提供多池路由和自动滚动发布。

`FLEET_MAX_ROOMS` 必须由真实 Linux 实例压测决定，开发示例的 `2` 仅便于验证扩缩容。`MIN_INSTANCES` 是最低实例数，不等于最低空闲房间数。初版为单区域、单构建、双人房间、保守的按需求扩容；详见 [容量策略](docs/architecture.md#capacity-policy)。

实际云端接入前还需要：PlayFlow 项目/构建版本、region、机型、UDP 端口名称，战斗服可访问的 HTTPS 控制地址，以及正确实现的游戏 Host。PlayFlow API key 和服务器凭证只进入服务端配置；客户端只得到自己的短期入场凭证。

## 参考与检索结论

检索时间：2026-09-21。公开 GitHub 仓库搜索与官方文档搜索未找到可直接采用、明确维护中的 PlayFlow–Nakama FleetManager 适配；这不代表私有项目或未被索引的实现不存在。

- [Edgegap Go FleetManager](https://github.com/edgegap/nakama-edgegap) 与 [Unity 插件](https://github.com/edgegap/edgegap-server-nakama-plugin-unity)：借鉴集成与发布结构。
- [PlayFlow UGS Matchmaker 指南](https://docs.playflowcloud.com/guides/ugs-matchmaker)：借鉴匹配与实例分配分离的流程；UGS 的 API 不能直接用于 Nakama。
- [PlayFlow API](https://docs.playflowcloud.com/api-reference/introduction)：客户端根据公开 v3 接口实现，实际调用基址为 `https://api.computeflow.cloud/api`。
- [Heroic Labs GameLift 实现](https://github.com/heroiclabs/nakama-gamelift)：官方 FleetManager 扩展参考。

本项目采用 MIT License。
