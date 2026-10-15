# 示例:CloudWeGo gomall

[gomall](https://github.com/cloudwego/biz-demo/tree/main/gomall) 是 CloudWeGo 官方的电商示例:8 个 Kitex 服务、一个 Hertz 前端，生成代码集中在公共模块 `rpc_gen` 里。这里把每个子目录当作一个仓库来扫描，模拟多仓库的场景。

| 文件 | 内容 |
|---|---|
| [workspace.json](workspace.json) | 工作区配置(10 个"仓库") |
| [scan.txt](scan.txt) | `scan` 的控制台输出 |
| [report.md](report.md) | 生成的报告，含服务依赖图 |
| [trace-checkout.txt](trace-checkout.txt) | `trace checkout:Checkout`:下单功能涉及的服务和调用链 |
| [cart.diff](cart.diff) / [impact-cart.txt](impact-cart.txt) | 模拟修改 cart 服务的一行代码，`impact` 给出的跨仓影响面 |

复现:

```bash
cd examples/gomall
git clone --depth 1 https://github.com/cloudwego/biz-demo.git
kitex-depgraph scan -c workspace.json -o out
kitex-depgraph trace -g out/graph.json checkout:Checkout
kitex-depgraph impact -g out/graph.json -repo cart -diff cart.diff
```
