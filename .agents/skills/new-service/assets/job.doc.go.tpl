// Package job 是__scope__的异步任务层：队列消费者、定时任务。消费者用 pkg/queue.Worker
// （实现 app.Component），在 cmd/<svc>/initial 的 createServer 里注册。
//
// 依赖 service 的方式同 handler：在本包里声明需要的窄用例接口，不依赖具体类型，
// 实现由 initial 注入。
package job
