# 验证记录

原型在两类代码上验证：仓库自带的演示工作区(自动化测试)，以及 CloudWeGo 官方的开源示例项目(真实代码，人工核对)。

## 1. 演示工作区(自动化测试)

[`testdata/demo`](../testdata/demo) 有 6 个仓库，每个场景都对应一条端到端测试断言(`internal/e2e`):

| 场景 | 仓库 | 期望结果 |
|---|---|---|
| 公共仓生成：调用方和实现方引用同一个生成包 | gateway、order-svc → idl ← user-svc | 按"共享生成代码"对齐 |
| 本仓生成：两边各自生成 order IDL | gateway、order-svc | 按 IDL 身份 `order.OrderService` 对齐 |
| 调用方的 IDL 比实现方旧，调用了已删除的方法 | gateway → `QueryOrderLegacy` | `missing-method`(高) |
| 两份生成代码方法集合不同 | gateway、order-svc 的 kitex_gen | `drift`(中) |
| 同一 IDL 两个实现方，按 destService 选择 | user-svc(user)、user-svc-v2(user-v2) | 写死 "user" 的调用只连到 user-svc |
| destService 来自函数参数 | order-svc `service.Audit` | `ambiguous`(中) |
| 调用的服务不在工作区 | order-svc → pay | `external`(提示) |
| handler 经接口变量注册 | order-svc `main.go` | CHA 找到 `handler.OrderServiceImpl` |
| 服务名是另一个包里的常量 | order-svc `conf.ServiceName` | 注册名求出 `order` |
| 客户端存在结构体字段、包级变量、局部变量里 | order-svc、gateway | destService 都能追到 |
| 客户端藏在接口后面(`Payer`) | order-svc `rpc.PayAdapter` | 调用链经 CHA 展开到 pay 调用 |
| 共享 SDK 里的调用 | common-sdk `usersdk` | 归属到调用它的 gateway;没人用的方法不计入 |
| IDL 线上方法名与 Go 方法名大小写不同 | pay `prepay` / `Prepay` | 按线上方法名对齐 |
| 公共 IDL 只在模块缓存或 vendor 里 | 临时构造 | 分别从 GOMODCACHE、vendor 读到生成代码 |
| 公共 IDL 哪里都找不到 | 临时构造 | 给出警告，说明哪些包缺失、被谁引用 |

另外有单元测试覆盖 go.mod 解析、生成代码头识别、v0.4 / v0.12 两种 serviceInfo 写法、diff 解析等。

## 2. CloudWeGo 开源项目(人工核对)

源码:[cloudwego/biz-demo](https://github.com/cloudwego/biz-demo)、[cloudwego/kitex-examples](https://github.com/cloudwego/kitex-examples)(2026-10-15 的 main 分支)。

### gomall(按 10 个仓库扫描)

配置见 [`examples/gomall/workspace.json`](../examples/gomall/workspace.json),输出见 [`examples/gomall`](../examples/gomall)。

- 生成代码在公共模块 `rpc_gen` 里(公共仓生成),Protobuf IDL,Kitex v0.8.0。
- 7 个服务(cart、checkout、email、order、payment、product、user)的生成代码和 7 处 `NewServer` 注册全部识别，各服务全部方法都定位到了实现函数。
- 业务代码里 10 处 `NewClient` 的 destService 都求出常量("product"、"cart" 等)。
- 人工用 `grep 'rpc\.[A-Za-z]*Client\.[A-Z]'` 统计业务 RPC 调用:cart 1 处、checkout 6 处、frontend 15 处，共 22 处;工具识别的结果完全一致，并全部对上实现方。
- `rpc_gen/rpc` 下有一套 cwgo 生成的通用客户端封装，业务代码没有使用。工具把这 14 处调用识别为库代码，不计入依赖图。
- 报告提示 email 服务"没有 RPC 调用方"。核对代码，gomall 通过消息队列(NATS)通知 email,确实没有 RPC 调用，提示正确。
- `trace checkout:Checkout` 展开出 checkout → cart、product、order、payment 的完整链路;模拟修改 cart 的 `GetCartService.Run`,`impact` 找出 checkout、frontend 两个上游，以及经 checkout 传递影响的 frontend 下单页面等入口。

### easy_note(一个仓库，多个 main 包)

- 自动按 main 包拆成 `cmd/api`、`cmd/note`、`cmd/user` 等组件。
- 服务注册名经 `server.WithServerBasicInfo` 从另一个包的常量(`consts.NoteServiceName = "demonote"`)求出;客户端侧的 `client.WithClientBasicInfo`(调用方自己的名字)不会被误当成注册名。
- 7 处调用(api → note、api → user、note → user)全部对上。

### bookinfo(wire 依赖注入)

- handler 通过 wire 注入的接口字段传给 `NewServer`。3 个实现都靠 CHA 找到。
- 验证中发现的问题：最初 details 服务的 handler 解析失败，因为同仓库一个 Hertz handler 类型也有同名方法 `GetProduct`。之后加入签名比对(参数、返回值类型在可知时必须一致)，问题消除。
- 3 处调用(productpage → details、productpage → reviews、reviews → ratings)与人工 grep 一致。

### kitex-examples(226 个包)

- 覆盖 Kitex v0.1.4 到 v0.16.1 共 13 个版本的生成代码，全部识别。
- 61 处服务注册，其中 60 处直接确定 handler 类型。
- 验证中发现的问题:`bizdemo/easy_note/pkg/constants/constant.go` 本身有语法错误(字符串字面量里有换行)。Go 解析器默认在 10 个错误后放弃，返回空文件，导致整个包被丢掉。改为 `AllErrors` 模式并忽略空文件后，同包另一个文件和出错位置之前的常量都能正常使用。
- 示例仓库里大量 server 实现同一个 `api.Echo` IDL,所以出现很多"多个候选实现方"提示。这符合预期，不是误报。

## 3. 性能

本地单进程，没有缓存:

| 代码 | 规模 | 耗时 |
|---|---|---|
| gomall(10 个仓库) | 123 个包，220 个文件 | 0.1s |
| kitex-examples | 226 个包，440 个文件 | 0.4s |
| Kitex 框架源码 | 140 个包，491 个文件 | 0.4s |
| biz-demo 整仓 | 1370 个包，2394 个文件 | 1~2s |

只读源码、依赖用占位包，所以速度主要取决于工作区本身的代码量，和依赖多少无关。

## 4. 尚未验证

- 公司内部仓库(proto / Thrift 混用、自研框架封装、PSM 风格服务名);
- 上万个包的超大工作区的内存占用;
- Windows 路径。
