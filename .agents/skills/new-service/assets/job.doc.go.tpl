// Package job 是 __svc__ 的异步任务层：队列消费者、定时任务。消费者用 pkg/queue.Worker
// （实现 app.Component），在 cmd/<svc>/initial 的 createServer 里注册。
package job
