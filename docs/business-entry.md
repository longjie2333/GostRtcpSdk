# 业务连接入口与 Gin 接入

`rtcp.Listen(ctx, cfg)` 返回标准 `net.Listener`。连接流程是：

```text
访客 → 公网 GOST 的绑定端口 → TLS / Relay / smux → net.Conn → 业务处理
```

业务进程主动连接公网 GOST，业务侧不监听 TCP 端口，也不需要 target。SDK 不依赖 Gin；同一入口可以传给 `http.Server.Serve`、其他接受 net.Listener 的服务，或自己的 Accept 循环。公网端仍需官方 GOST，启用 `bind=true`，并允许访问所绑定的端口。

## 通用 TCP 业务

```go
listener, err := rtcp.Listen(ctx, cfg)
if err != nil { return err }
defer listener.Close()
for {
    conn, err := listener.Accept()
    if err != nil {
        if errors.Is(err, net.ErrClosed) { return nil }
        return err
    }
    go func(conn net.Conn) {
        defer conn.Close()
        // 在这里使用 conn.Read / conn.Write 实现业务协议。
        // 示例回显使用标准库，避免在业务端再建立 TCP 连接。
        io.Copy(conn, conn)
    }(conn)
}
```

这是宿主函数内的接入片段，`ctx` 与 `cfg` 由宿主管理。业务必须关闭每条已接收连接，自己管理业务 goroutine。只有 TCP 字节流语义，不提供 UDP 或 TCP 半关闭扩展。

## Gin 示例

[可直接运行的独立消费模块](../src/examples/gin/main.go) 使用 Gin v1.11.0，SDK 通过它自己的 `go.mod` 中的 replace 引用。它演示：

- 启动时同步完成远端绑定，直接返回认证、网络和绑定错误。
- 将 Gin 引擎作为 http.Handler 交给 `http.Server.Serve(listener)`。
- `/ping` 返回 JSON 与访客 IP，`/echo` 接收并返回 JSON。
- 先 `http.Server.Shutdown`，再取消隧道 context，保留正在处理的请求。

示例的 `serve(stop, cfg, handler, logger)` 接受任意 http.Handler。其中监听 context 与停止信号分离：启动阶段的停止信号可以取消 Listen，启动完成后停止信号触发 HTTP 排空，直到排空结束才取消隧道。不能将随 Ctrl+C 立即取消的 context 直接当成整个服务的隧道 context，同时又期望正在处理的请求正常完成。

## JmptJwxtAPI 的对应位置（只读参考）

核对的是 `C:\.Projects\JmptJwxtAPI` 当前代码，没有改动该项目，也没有启动 OCR 或访问真实教务系统。

| 现有位置 | 当前职责 | 采用 SDK 时的接入点 |
|---|---|---|
| `cmd/jwxt/main.go` 的 `run` | 构造 OCR、教务客户端、httpapi.Server，最后调用 serve | 继续用已经构造好的 `api` 作为 handler，把 GOST Config 传给 serve |
| `cmd/jwxt/server.go` 的 `serve` | `net.Listen("tcp", address)`，配置 http.Server，再调用 Serve 和 Shutdown | 使用 SDK 的 Listen 取得 listener，并采用示例中的隧道 context 生命周期 |
| `internal/httpapi/server.go` 的 `Server.ServeHTTP` | 将请求交给内部 Gin Engine | 已符合 http.Handler，无需暴露内部 router 或修改路由、中间件 |
| `internal/config/config.go` | 宿主集中管理配置 | 实际接入时由宿主添加服务器、绑定地址和认证配置，不让 SDK 读取业务项目环境变量 |

在该项目中，最终组装调用可以写成下面的形式；配置值应由其配置模块读取：

```go
gostCfg := rtcp.Config{
    Server: "relay.example.com:1080",
    Bind:   "0.0.0.0:8080",
    User:   url.UserPassword(user, password),
    Logger: logger,
}
return serve(stop, gostCfg, api, logger)
```

`serve` 的完整实现见 Gin 示例，不需要修改 `httpapi.New`、`api.ServeHTTP`、接口路径或业务返回值。示例保留了参考项目的 HTTP 头读取 5 秒、读取 10 秒、写入 20 秒、空闲 60 秒、最大头 16 KiB、关闭排空 10 秒的参数及请求 BaseContext。实际集成时继续保持先关闭 HTTP 服务和 API、再释放教务客户端与 OCR 的资源顺序。

`conn.RemoteAddr()` 使用官方 Relay 提供的访客地址，因此 net/http 的 `Request.RemoteAddr` 和 Gin 的 `ClientIP()` 可取得访客 IP。参考项目的 `SetTrustedProxies(nil)` 可以保持。隧道 TLS 只保护 SDK 到公网 GOST 的链路；HTTP 层的 TLS 终止仍由部署方式决定。

## 端口与停止语义

- `Bind: "0.0.0.0:0"` 请求公网端分配空闲端口；Listen 返回后即可读取 `listener.Addr()`。不要使用 `:8080`，保留的源版空主机缺陷仍适用。
- `Addr()` 是绑定地址，不一定是访客可直接使用的公网地址。例如 `0.0.0.0:12345` 应使用服务器的公网 IP 或域名加 `12345` 访问。
- 断线后自动重连、重新绑定；自动分配的端口可能改变，绑定日志及 Addr 会更新。需要固定入口时指定固定端口。
- `listener.Close()` 终止 Accept，已交付连接继续工作；最后一条连接关闭后释放隧道和公网端口。这个等待期间不会交付新的业务连接。
- 取消传给 Listen 的 context 会强制终止隧道及所有连接。HTTP Shutdown 超时、劫持连接尚未释放等情况可用它最终回收资源。
- 不支持恢复已中断的请求或 TCP 会话。共享 TLS/smux 故障仍会影响同一会话的所有连接。

## 验证范围

`scripts/check.ps1` 会分别检查 SDK 和独立 Gin module，使用官方 GOST v3.3.0 运行互通测试，完整模式拒绝 SKIP。Gin 测试覆盖真实公网端口链路上的 GET、JSON POST、ClientIP 以及正在处理的请求正常收尾；SDK 测试另行覆盖连接生命周期、协议头、并发、地址和重连。

这验证的是 Gin 和现有 http.Handler 结构的接入方式，不等同于已经修改或端到端验证 JmptJwxtAPI 的教务、OCR、认证业务。
