# 源码基线与行为边界

SDK 精简自 [go-gost/x v0.16.0](https://github.com/go-gost/x/tree/14fed91c6245289e2e92ea60a97332a62c90fad0)，对应官方 GOST v3.3.0。原链路为 `TLS → Relay BIND → smux → TCP 目标`。客户端调用 smux.Server 是因为远端主动 OpenStream，不表示 SDK 提供公网服务端。

## 已保留的行为

- 官方服务端需要 `bind=true`；认证、权限和实际远端监听由官方程序处理。
- 首次 Relay 回复确定绑定地址；每条 stream 的第二次回复先消费，再交付业务字节，并解析访客地址与可选目标覆盖。
- 使用原版 Relay v0.7.0 和 smux v1.5.31。默认 smux v1，保活 10 秒、超时 30 秒；共享 TCP 丢包和会话故障会影响多个 stream。
- stream 关闭只影响对应连接；取消 Run 关闭 session/TLS，官方服务端随会话退出释放绑定端口。
- 双向复制等待两个方向结束。目标 TCP 可 CloseWrite，余下读取设置 10 秒期限；smux v1.5.31 没有 CloseWrite，关闭的是整个 stream。因此不承诺 FIN 后的尾部响应能穿过完整链路。
- 默认 TLS 不验证服务端身份，可通过 Config.TLS 显式启用。

## 有意保留的源版本缺陷

1. `:8080` 会经过错误拼接变成 `0.0.0.0::8080` 并绑定失败。使用显式 `0.0.0.0:8080`，不提供自动兼容或纠错。
2. 源复制循环先处理 Read 错误，所以 `Read(n>0, io.EOF)` 的那次尾部字节被丢弃。
3. 成功短写不会补写。这两项通过可控连接测试保留，不在工程整理中暗中修复。

如需改变这些行为，应建立独立变更、更新测试和说明，并按 AGENTS.md 的 PR 流程处理。

## 已删除的范围

不提供服务端、UDP、通用 CONNECT、SOCKS/SSH、任意代理链、通用配置热重载、管理 API、公开 Listener/Conn、旧 CLI 或旧 API 适配。公开 Config、NewClient、Client.Run 和 Client.UpdateTarget；仅默认 target 支持热更新。原版公开连接包装的共享 deadline 入口也随接口删除，宿主不能直接操作 stream。

日志和 context 生命周期是面向 SDK 的接口，不模拟整个 GOST 框架的指标、记录器或进程管理。SDK 只维护当前已声明的单节点 TCP 客户端行为，不声称与所有 GOST 历史版本和任意配置等价。

## SDK 增量行为

UpdateTarget 使用并发安全快照，新流选新目标，已有连接不迁移。不改变 Relay/smux 协议或远端监听。目标语法要求非空 host 和有效数字端口，非法更新不改变旧值；构造函数使用同一校验。
