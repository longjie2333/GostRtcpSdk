# 配置与 API

包名是 `rtcp`，module 标识为 `example.com/gostrtcpsdk`，导入路径为 `example.com/gostrtcpsdk/src`。

```go
func Run(ctx context.Context, cfg Config, target string) error
```

Run 会阻塞，适合宿主独立 goroutine 或前台服务调用；没有额外的 SDK Client、Start/Stop、全局单例或服务端对象。

| 参数/字段 | 类型 | 要求与默认行为 |
|---|---|---|
| `ctx` | `context.Context` | 非 nil，由宿主管理；取消时结束连接和转发，返回取消错误 |
| `cfg.Server` | `string` | 官方 Relay+TLS 服务地址 `host:port`，不带 URL scheme；格式错误直接返回 |
| `cfg.Bind` | `string` | 远端监听地址，显式填写 IP 和端口，如 `0.0.0.0:8080`；不能依赖空主机形式 |
| `cfg.User` | `*url.Userinfo` | 使用 `url.UserPassword` 设置认证；nil 不发送认证 Feature，对端可能拒绝 |
| `cfg.TLS` | `*tls.Config` | nil 时默认不验证证书；非 nil 会 Clone；未设置 ServerName 时从 Server 取主机名 |
| `cfg.Logger` | `*slog.Logger` | nil 时使用 `slog.Default()`；Info 输出实际绑定地址，Debug 输出访客与转发事件 |
| `target` | `string` | 从 SDK 所在机器拨号的 TCP 目标，如 `127.0.0.1:80`；必须符合 `host:port` 格式 |

配置及其引用对象在一次 Run 期间保持不变。需要改配置时先取消并等待旧 Run 返回，再以新配置启动。多个独立转发可由宿主调用多个 Run，各自使用上下文及不同远端端口。

## 证书验证

```go
roots, err := x509.SystemCertPool()
if err != nil { return err }
cfg.TLS = &tls.Config{
    RootCAs: roots,
    ServerName: "relay.example.com",
}
```

私有 CA 可用 `x509.NewCertPool()` 和 `AppendCertsFromPEM` 加载。凭据由宿主的配置系统或环境提供，不建议写入源码或日志。SDK 只加密客户端到官方 GOST 的 TLS 链路。

## 运行状态和错误

- 调用 Run 不表示远端已经完成绑定；Info 的 `remote TCP listening` 表示本次绑定成功，并包含分配出的真实端口（支持显式 `127.0.0.1:0`）。
- TLS/绑定失败按 `1、2、4、5` 秒退避持续重试，并记录错误，包括认证失败、对端没有启用 bind、端口冲突。它们不会作为一次 Run 的永久错误直接返回；宿主需按业务要求通过 context 限定时间或终止。
- 会话或访客回复异常会关闭会话，1 秒后尝试重新绑定；已有 TCP 会话不会恢复，也不会重放业务字节。
- 目标 TCP 拨号默认 15 秒超时，只尝试一次；失败仅关闭对应 stream。
- TCP/TLS 建立默认 15 秒期限。BIND 回复等待由调用方 context 控制；不额外增加业务空闲超时。
- 取消后等待 Run 返回，才代表 SDK 已完成清理。用 `errors.Is(err, context.Canceled)` / `context.DeadlineExceeded` 区分调用方主动停止。
- 远端第二个可选 AddrFeature 可覆盖 target，与源版本保持一致；SDK 信任所连接的官方服务端。

## 示例环境变量

`go run ./src/examples/basic` 从 `GOST_SERVER`、`GOST_BIND`、`GOST_TARGET`、`GOST_USER`、`GOST_PASSWORD` 读取配置。前三项必填；认证按需设置。示例只负责参数读取和 context，不属于 SDK 的额外配置框架。
