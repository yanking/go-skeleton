---
name: new-module
description: 在已按模块分目录的服务里新增业务模块：用脚本渲染 internal/<svc>/<module>/ 的分层骨架（各层 doc.go），再按清单划错误码子段、写 proto、接线。用户说「加个 xxx 模块 / 新增一个业务模块 / 在 <svc> 里加一个业务域」时使用；服务还是平铺布局时脚本会拒绝，先迁成模块。
---

# 新增模块

布局约定与模块边界以 AGENTS.md「服务分层」为准；这里只负责把新模块的骨架落到正确位置。

## 生成骨架

```bash
bash .agents/skills/new-module/scripts/new_module.sh <svc> <module>
```

- `<module>` 是小写字母开头的字母数字串（如 `order`），不能与层名（handler/service/repo/model/job）或 `config` 重名，也不能用 Go 工具会特殊对待的 `testdata`、`vendor`、`internal`；同名模块已存在时拒绝覆盖。
- 生成 `internal/<svc>/<module>/` 下 `service/repo/model/job` 的 `doc.go`；服务有传输段（`configs/<svc>.yaml` 里有 `transport:`）时额外生成 `handler/doc.go`。包注释模板与 new-service 共用。
- 服务还是平铺布局（`internal/<svc>/` 下直接有分层目录）时拒绝执行：先按 AGENTS.md「服务分层」把现有代码迁成模块，再加新模块。

## 生成后必做（顺序即检查清单）

1. **划错误码子段**：AGENTS.md 错误码表本服务那一行，给新模块划一个未用的千位子段。
2. **写 proto 契约**（服务有传输段时）：一个模块一个文件 `api/<svc>/v1/<module>.proto`，各自声明一个 gRPC service；注释格式按 `.agents/go-style.md`「proto 注释」，写完 `make proto`。
3. **填业务实体**：各层填什么见其 doc.go。要用别的模块的能力，在本模块的 service 里声明窄接口，由 initial 传入对方 service 的实现；不引用对方的 handler、repo、model、job。
4. **接线**：在 `cmd/<svc>/initial` 的 createServer 里构造本模块的 repo → service → handler，`WithService` 追加一次，服务有 HTTP 出口时 `WithGateway` 也追加一次；同层包按 `<模块><层>` 起别名（如 `orderservice`）。服务有自己的鉴权拦截器时，新方法默认要登录，要公开就按该拦截器的规则登记。
5. **建表**（有库时）：`make migrate-create SVC=<svc> NAME=create_<表名>`，迁移仍放 `migrations/<svc>/`。
6. **验证**：`make check` 全绿——其中 `test/arch` 的架构测试检查模块边界。

## 约束

- 只动新模块的文件，以及清单里点名要改的地方。
