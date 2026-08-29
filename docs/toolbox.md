# 工具参考（仓库级）

> 面向在本仓库干活的 AI 助手与新人。make 目标看 `make help`，技能看 `.agents/skills/`，
> 提交门槛看宪法第 5 条；此处只放别处没有的：archify 画图与手工工具。

## archify（架构图 / 时序图）

架构图与时序图由 [archify](https://github.com/tt-a1i/archify) 生成：输入类型化 JSON 图源，
输出自包含 HTML（内联 SVG、明暗主题、可交互、可导出 PNG/SVG）。图源与产物随服务文档放
`docs/<svc>/diagrams/`：`<名>.json`（可编辑图源）→ `<名>.html`（冻结产物）。

**改图流程**（只改 JSON 图源，HTML 由 deliver 重新生成）：

```sh
cd <archify 目录>
node bin/archify.mjs validate <type> <图源.json> --quality showcase --json   # type: architecture|sequence|workflow|dataflow|lifecycle
node bin/archify.mjs deliver   <type> <图源.json> <产物.html> --quality showcase --json
```

- `validate` 须 9 项 artifact 检查全过（0 错误 0 警告）；报错按 diagnostics 的 `supportedFixes` 修图源。
- `deliver` 原子写出 HTML 并在图源旁留快照；产物提交进仓库，文档里直接相对链接。
- 类型选法：组件/边界 → `architecture`；调用链/生命周期 → `sequence`；状态机 → `lifecycle`；
  拿不准先 `node bin/archify.mjs guide "<场景>" --json`。

**安装**：需要 Node.js；`git clone https://github.com/tt-a1i/archify` 到本地技能目录
（如 `~/.agents/skills/archify`），`node bin/archify.mjs doctor` 验证就绪。已装为 agent 技能的
环境里，直接说「用 archify 画一张 … 时序图」即可触发。

## 手工工具（e2e 与冒烟前置）

`grpcurl`（gRPC 冒烟与 e2e 断言；就绪探测 `grpcurl -plaintext 127.0.0.1:<grpc端口> list`）、`jq`、`psql`。
