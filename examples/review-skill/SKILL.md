---
name: kitex-cross-repo-review
description: 评审 Kitex 微服务的代码改动时，用 kitex-depgraph 分析跨仓库影响面(受影响的 RPC 接口、上游调用方、IDL 变更)，写进评审意见。改动涉及 handler、service 逻辑、kitex_gen 或 RPC 调用时使用。
---

# Kitex 跨仓影响面评审

## 前提

- 已安装 `kitex-depgraph`,并有一份较新的 `graph.json`(由 `kitex-depgraph scan -c <工作区配置>` 生成)。默认路径 `depgraph-out/graph.json`,也可以用环境变量 `KDG_GRAPH` 指定。
- 知道改动所在的仓库名，和工作区配置里的 `name` 一致。

## 步骤

1. 对改动运行影响分析:

   ```bash
   git diff -U0 <基线分支>...HEAD | kitex-depgraph impact -g "${KDG_GRAPH:-depgraph-out/graph.json}" -repo <仓库名> -diff - -json
   ```

2. 读输出的 JSON:
   - `idlChanges`:生成代码有改动，优先级最高。列出全部实现方和调用方组件。
   - `affectedRPCs`:受影响的接口。`level` 为 1 表示本仓库直接提供的接口，2 及以上表示经上游服务传递的影响;`callers` 是各调用点。
   - `entries`:受影响的 HTTP 路由处理函数等入口，作为回归测试范围。
   - `components`:需要知会或回归的服务。
3. 如果 `graph.json` 的 `scannedAt` 比基线分支最近一次提交早一天以上，提醒重新执行 scan,本次结果可能过时。

## 写进评审意见

- 先给一段"跨仓影响"小结：受影响接口数、上游组件列表、有没有 IDL 变更。
- `level` 为 1 且有其他组件调用的接口：检查改动是否改变了接口语义(返回值、错误码、耗时、幂等性)。有变化就要求在 PR 描述里说明，并知会调用方。
- 有 IDL 变更：要求列出各调用方的升级计划;graph.json 里已有的 `missing-method` 问题直接标为阻塞。
- 结果来自静态分析，接口调用按 CHA 展开，可能多报。不要把每条都当成确定的依赖，关键链路用 `kitex-depgraph trace` 复核。
