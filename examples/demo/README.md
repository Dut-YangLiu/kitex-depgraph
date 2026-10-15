# 示例：演示工作区

基于 [`testdata/demo`](../../testdata/demo) 的输出。工作区有 6 个仓库，场景说明见 [docs/validation.md](../../docs/validation.md#1-演示工作区自动化测试)。

| 文件 | 命令 |
|---|---|
| [scan.txt](scan.txt)、[report.md](report.md) | `kitex-depgraph scan -c testdata/demo/workspace.json` |
| [trace.txt](trace.txt) | `kitex-depgraph trace gateway:CreateOrder` |
| [trace.mmd](trace.mmd) | `kitex-depgraph trace -format mermaid gateway:CreateOrder` |
| [callers.txt](callers.txt) | `kitex-depgraph callers user.UserService/GetUser` |
| [impact.txt](impact.txt) | `kitex-depgraph impact -repo user-svc -diff examples/demo/user-svc.diff` |

`trace.mmd` 渲染出来是这样(按仓库分组，粗箭头是 RPC 调用):

```mermaid
flowchart TD
  subgraph c0["gateway"]
    f0["handler.CreateOrder"]
    f1["rpc.CreateOrder"]
  end
  subgraph c1["order-svc"]
    f2["(*handler.OrderServiceImpl).CreateOrder"]
    f3["(*service.OrderService).Create"]
    f5["(rpc.PayAdapter).Prepay"]
  end
  subgraph c2["user-svc"]
    f4["(*main.UserServiceImpl).GetUser"]
  end
  f6["pay.PayService(工作区外的服务)"]
  f0 --> f1
  f1 ==>|"order.OrderService/CreateOrder"| f2
  f2 --> f3
  f3 ==>|"user.UserService/GetUser"| f4
  f3 -.-> f5
  f5 -.->|"pay.PayService/prepay"| f6
```
