# AGENTS.md

面向所有 AI 编码代理的仓库入口指南。

> **仓库状态**：脚手架已跑通。`pkg/` 下 conf、app、log、telemetry、transport、mysql、postgres、redis 八个包；Makefile ＋ buf ＋ sqlc 构建链完整；首个服务 `user` 四层落地，双协议、参数校验、优雅停机与 `trace_id` 入日志均已实测。后续新建服务照 `.agent/engineering.md`「新建服务清单」走。发现文档与现实不符须当轮报告并修正其一（宪法第四条）。

## 宪法（必读，最先加载）

立即完整阅读 `CONSTITUTION.md`——Claude Code 由下行 `@` 自动加载，其他工具请直接打开该文件。位阶与冲突裁决以宪法为唯一定义。

@CONSTITUTION.md

## 项目定位

Go 微服务开发脚手架，**多服务 monorepo**：`.proto` 是对外协议的唯一事实源，每个服务经 [grpc-gateway](https://github.com/grpc-ecosystem/grpc-gateway) 在同一进程内提供 gRPC 与 HTTP 双协议，OpenAPI 接口文档由 pb 文件自动生成，不手写接口文档。

## 文档路由（按需强制加载，宪法第二条）

不预读全部文档；任务命中下表某行时，该行文档是强制前置阅读：

| 任务涉及 | 加载 |
|---|---|
| 新建一个服务 | `.agent/engineering.md`「新建服务清单」＋`.agent/architecture.md`「目录契约」 |
| 新增/修改 `.proto`、接口与错误设计 | `.agent/architecture.md`「协议层规则」＋`.agent/engineering.md`「Proto 工作流」 |
| 写或改 Go 代码 | `.agent/go-style.md`＋`.agent/architecture.md`「依赖方向（红线）」 |
| 新增模块、目录调整、跨层/跨服务调用 | `.agent/architecture.md` |
| 新增/修改 SQL、迁移、仓储实现 | `.agent/engineering.md`「SQL 工作流」＋`.agent/architecture.md`「依赖方向（红线）」 |
| 构建、生成、测试、lint、迁移、提交 | `.agent/engineering.md` |
| 以上均不命中 | 停下，走宪法第三条向用户确认 |

## 命令

命令契约（init / api / breaking / build / run / test / lint / migrate / all 及单测单跑）唯一定义在 `.agent/engineering.md`「命令契约（Makefile）」，不在此复制。
