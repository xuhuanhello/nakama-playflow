# Nakama PlayFlow FleetManager

将 Nakama 社区版的匹配结果分配到 PlayFlow 托管的 Unity Linux 战斗服，支持一个进程承载多个双人房间。配套使用 [Unity Server Agent](https://github.com/xuhuanhello/playflow-server-nakama-plugin-unity)。

社区项目，非 Heroic Labs、PlayFlow 或 Edgegap 官方插件。当前为 `0.1.0-dev`，已完成本地集成验证，真实云端和游戏接入仍需验收。

## 实现了什么

- 实现官方 Go FleetManager 接口及双人 Matchmaker hook，无需修改 Nakama 源码。
- 对接 PlayFlow v3 实例创建、查询和停止，按房间容量分配玩家并签发短期入场凭证。
- 持久化实例、房间、席位和控制命令，支持心跳、取消确认、重连凭证及重启恢复。
- 按待分配需求扩容；排空后缩容，等待活跃房间和待提交结果处理完毕。

设计和能力边界见 [架构说明](docs/architecture.md)，已执行的检查见 [验证记录](docs/validation-2026-09-21.md)。

## 依据与参考

- [Heroic Labs GameLift](https://github.com/heroiclabs/nakama-gamelift)：官方 FleetManager 扩展方式。
- [Edgegap Go](https://github.com/edgegap/nakama-edgegap) / [Unity](https://github.com/edgegap/edgegap-server-nakama-plugin-unity)：服务端适配器与 Unity 包的配套结构。
- [PlayFlow API](https://docs.playflowcloud.com/api-reference/introduction) / [UGS Matchmaker 指南](https://docs.playflowcloud.com/guides/ugs-matchmaker)：实例管理接口及匹配、分配流程。

## 如何使用

本地启动需要 Docker（Compose、Buildx）和 make：

```sh
git clone https://github.com/xuhuanhello/nakama-playflow.git
cd nakama-playflow
make local-up
```

Nakama API：`http://127.0.0.1:17350`；Console：`http://127.0.0.1:17351`。此配置使用 PlayFlow mock，**不会创建云端实例**。登录信息、日志和清理命令见 [本地部署](docs/compatibility.md#running-locally)。

安装 Go 1.27.1、Node.js 22+ 和 curl 后，可运行：

```sh
make test test-race
make integration
```

接入真实 PlayFlow：

1. 用 `make package` 构建插件，按 [官方镜像安装说明](docs/compatibility.md#installing-with-the-official-image) 部署，并先执行 fleet 数据库迁移。
2. 按 [生产配置模板](deploy/production.env.example) 配置 PlayFlow 凭证、构建版本、区域、机型、端口及云端可访问的 HTTPS 控制地址。
3. 安装配套 Unity 包，实现游戏的房间管理、FishNet 入场认证、重连及结果持久化。具体职责见 [游戏接入清单](docs/game-integration.md)。

已有 Go runtime 工程可调用 `fleetmanager.RegisterFromEnv` 组合接入，详见 [兼容与构建说明](docs/compatibility.md)。

## 注意事项

- 当前构建目标为 **Nakama 3.41.0 / linux/amd64**。Go `.so` 必须匹配 Nakama 的工具链和共享依赖；升级后需要重新构建、实际加载验证。Unity 包不受 Go ABI 绑定。
- 初版为单区域、单构建、双人房间。一个部署 namespace 的实例配置固定；更换构建、容量或签名主密钥需新建 namespace。
- 官方 Nakama Unity SDK 无需修改，但游戏必须完成 Host 适配和连接流程。不要同时加载两个接管默认 FleetManager 或 Matchmaker hook 的插件。
- 房间上限、扩缩容阈值和实例 TTL 需通过真实 Linux 战斗服压测确定；示例值不是容量承诺。
- 本地 Compose 仅用于开发。真实凭证只放入服务端密钥配置，不进入 Git、Unity 客户端或日志。安全事项见 [SECURITY.md](SECURITY.md)。
- `main` 为开发分支；可重复部署请固定经过验证的 commit SHA。

## 协议与许可证

控制接口及入场凭证使用 [Fleet Protocol v1](docs/protocol-v1.md)。代码采用 [MIT License](LICENSE)。
