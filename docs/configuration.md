# 配置与 API

包名是 `rtcp`，module 标识为 `example.com/gostrtcpsdk`，导入路径为 `example.com/gostrtcpsdk/src`。

```go
func NewClient(cfg Config, target string) (*Client, error)
func (c *Client) Run(ctx context.Context) error
func (c *Client) UpdateTarget(target string) error
```

NewClient 不建立网络连接。Client.Run 会阻塞，同一实例只允许一个 Run 同时执行，重复调用返回错误。取消并等 Run 返回后可再次运行，保留最新目标。必须通过 NewClient 构造，实例不可复制；没有额外 Start/Stop 或全局单例。

| 参数/字段 | 类型 | 要求与默认行为 |
|---|---|---|
| `ctx` | `context.Context` | 非 nil，由宿主管理；取消时结束连接和转发，返回取消错误 |
| `cfg.Server` | `string` | 官方 Relay+TLS 服务地址 `host:port`，不带 URL scheme；格式错误直接返回 |
| `cfg.Bind` | `string` | 远端监听地址，显式填写 IP 和端口，如 `0.0.0.0:8080`；不能依赖空主机形式 |
| `cfg.User` | `*url.Userinfo` | 使用 `url.UserPassword` 设置认证；nil 不发送认证 Feature，对端可能拒绝 |
| `cfg.TLS` | `*tls.Config` | nil 时默认不验证证书；非 nil 会 Clone；未设置 ServerName 时从 Server 取主机名 |
| `cfg.Logger` | `*slog.Logger` | nil 时使用 `slog.Default()`；Info 输出实际绑定地址，Debug 输出访客与转发事件 |
| `target` | `string` | 从 SDK 所在机器拨号的 TCP 目标，如 `127.0.0.1:80`；host 必须非空，端口为 1..65535 的数字，构造与更新使用同一校验 |

Config 及引用对象在实例使用期间保持不变。仅 target 可通过 UpdateTarget 热更新；修改服务器、认证或绑定配置需取消旧实例并创建新实例。多个独立转发使用不同 Client、上下文和远端端口。

## target 热更新

- 可在 Run 前、运行中、重连期间或停止后更新；校验失败返回错误，旧目标不变。不会做 DNS 查询或连通探测。
- 更新与读取由锁保护；并发更新以实际写入顺序为准，每条流只读取一次完整快照。
- 选择时点是解析访客回复之后、启动转发 goroutine 之前；已经选定目标的拨号和已有连接不迁移、不打断。
- 远端可选第二 AddrFeature 仍覆盖默认 target；UpdateTarget 不改变这一源协议语义。
- 暂时不可达但语法正确的目标可以更新成功，实际拨号失败时仅关闭对应流。
- 旧包级 Run 删除，使用 NewClient → Client.Run。

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
