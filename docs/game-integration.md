# Unity/FishNet game integration

本插件面向一个 Unity Linux/FishNet 进程承载多个双人房间的游戏。Nakama 官方 Unity SDK 和 FishNet 源码可以保持不变；游戏需要实现适配代码，将自己的房间、连接、身份、重连和结算流程接入插件提供的接口。

Server Agent 负责通用控制协议。游戏实现 `IFleetServerHost`，负责真正创建房间、执行命令和报告状态。客户端通过官方 Nakama SDK 登录、匹配和查询分配，再连接分配到的 FishNet UDP endpoint。

## 接入职责

| 游戏侧职责 | 接入要求 |
| --- | --- |
| 房间生命周期 | 按 room/allocation/epoch 准备完整双人房间并保存名单；成功准备即占用容量，不能等第一个玩家连接时才争抢资源 |
| 客户端匹配流程 | 收到 Matchmaker 事件后查询 `fleet_assignment_get_v1`；等待状态变为 `assigned`，再将返回的 endpoint 接入所选 FishNet transport |
| 连接认证 | 在绑定连接与座位前调用 Agent 的入场验证，再检查本地房间、名单、epoch 和连接所有权；未经认证的连接不能访问任何房间 |
| 房间隔离 | 将每个连接映射到已认证的 room/seat；FishNet 对象可见性、消息路由和服务端指令都必须按房间隔离 |
| 模拟任务 | 报告排队数、执行数、最老等待时长和主线程帧耗时；仍在执行模拟任务的实例不可回收 |
| 结果与复核 | 报告待处理/执行中复核及尚未获得外部持久化确认的结果；容器本地磁盘写入不能作为安全销毁的依据 |
| 连续对局 | `between_rounds` 继续占用房间；drain 后停止开启新一局，并按约定完成当前局 |
| 断线重连 | 保留同一 allocation/room/seat；使用 `fleet_resume_v1` 获得新的一次性凭证，再执行游戏状态恢复和确认 |

一个房间必须只有一个席位分配权威。如果已有匹配或房间服务，迁移时先明确分配责任，在测试环境验证旧入口不再为同一房间签发席位后，再切换生产流量。

## Server Host contract

1. Linux Server 启动时读取 PlayFlow 注入的 `FLEET_*` 环境变量。先绑定 FishNet UDP listener、预加载房间共享资源并建立结果持久化能力，再报告 `IsReady=true`。
2. Agent 发起 bootstrap，生成本进程唯一 boot ID，随后每 2 秒主动心跳，不要求给 Unity 进程暴露管理 HTTP 端口。
3. `PrepareRoom` 必须快速同步完成；耗时资源准备放到启动阶段。执行前验证本地房间硬上限；成功后保留 roster 与 epoch。
4. 收到客户端入场票据后验证 HMAC、expiry、nonce、用户、worker、boot、room、allocation、reservation、seat、epoch。将 Nakama 用户 ID 映射到游戏身份体系，不能将未经验证的客户端 device ID 当作同一身份。
5. `CaptureRooms` 上报实际连接用户，不能把预留名单当成在线人数。暂时断线保留席位；新票据也不能绕过旧连接替换策略。
6. `CancelRoom` 在关闭房间、撤销入场且处理完其结果义务后才成功。重复命令幂等，过期的清理命令仍执行。失败后控制器生成新 command ID 重试。
7. 关闭后保留 `closed` 快照直到 ACK；ACK 前仍占本地容量。Agent 先通知快照 ACK，再处理响应中的新 prepare。
8. `BeginDrain` 只进入排空状态。停止新房间和新一轮，保留已允许的重连；等待活跃对局和外部结果持久化。Agent 不直接退出游戏进程。

Actor/room 的读写应保持 Unity 主线程所有权。后台模拟、复核和 HTTP 完成通过线程安全队列回传，不能让数据库或网络等待阻塞 `PrepareRoom`、心跳快照或游戏帧。

Agent 的断联保护会阻止新的入场验证；已开始的游戏如何继续、断线玩家保留多久、何时结束房间，应由游戏明确实现，并与控制器的重连窗口一致。签名验证本身不会恢复游戏状态，也不会替换已有 FishNet 连接。

## 结果持久化与排空

自动缩容依赖 Host 对剩余工作的真实报告。对局结束后，使用 allocation/room/epoch 等稳定标识把结果或回放幂等写入外部存储；收到持久化确认后，才允许相应待提交计数归零。结果服务不可用时保留任务并重试。

房间关闭不能丢弃尚未移交的结果责任。游戏应选择继续保留房间直到移交完成，或将任务可靠移交给独立持久化队列，并在真正安全之后报告 `closed`。进程重启后恢复对局、排名结算和回放存储都需要游戏另行实现。

## Cloud acceptance inputs

接入时确定 PlayFlow 构建版本整数 ID、region、机型、UDP 端口名与内部监听端口、战斗服可达的 HTTPS Nakama 控制地址、fleet 数据库、服务端密钥和游戏 build hash。真实密钥通过部署环境或密钥管理系统注入，不能提交到 Git 仓库，也不能放入客户端包。

当前 provider 使用构建中配置的端口，不覆盖它；`FLEET_PORT_NAME` 必须匹配返回的 `network_ports[].name`。客户端连接 `host:external_port`，不是 Unity 监听的内部端口。构建的默认启动命令须能启动无图形 Linux Dedicated Server；容器 running 与游戏 ready 分开验收。

首轮只开一个小池，验证两位真实客户端可以同时进入同房间、两个房间可以共存于同进程、跨房消息不能串流、断线重连与服务器重启不混淆。随后测试准备失败、用户不入场、重复票据、旧连接替换、Nakama 重启、Agent 短断网、PlayFlow 超时以及带结果待上传的 drain。

## Capacity acceptance

每个候选 `MaxRooms` 在真实目标机型上运行同步模拟峰值与正常玩家节奏两组负载，并同时开启游戏需要的复核模式。记录模拟队列等待 P95/P99、最老任务、主线程帧 P99、RSS、复核完成时间及结果上传重试。

将房间硬上限、模拟并行数、复核并行数分别标定。玩家暂时没有操作时，房间仍被占用；瞬间 CPU 低占用也不足以决定缩容。空闲计时只在房间已释放后开始。上线时先维持保守上限，再根据实测数据启用更积极的扩容预测和暖容量。

同时确认 PlayFlow 套餐是否强制进程 TTL。`FLEET_MIN_LIFETIME_FOR_ADMISSION_SECONDS` 默认 1800 秒，在剩余寿命不足时停止接新房间并排空；必须按最长房间寿命、重连和落盘上界调整，不能拿平均局长代替。房间无限续局会突破任何有限 TTL，游戏必须限制新一轮并在云端截止前完成结果移交。

例如把整间房寿命上限设为 30 分钟时，还需另计 90 秒重连和结果提交余量，1800 秒默认值便不够。2100 秒可作为该假设下的验证起点，最终仍由游戏硬上限和真实上传延迟决定。

完整消息字段见 [协议 v1](protocol-v1.md)，当前控制器边界与后续工作见 [架构说明](architecture.md)。Unity Agent、Host 接口与样例在 [配套 Unity 仓库](https://github.com/xuhuanhello/playflow-server-nakama-plugin-unity)。
