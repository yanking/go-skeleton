// Package job 是 payment 的异步任务层：三个定时组件——实例同步（NewSync）、滞留单
// 兜底（NewOrderSweep）、通知重投（NewNotifySweep）——共用 periodic 循环骨架，均实现
// app.Component，由 cmd/payment/initial 注册。
//
// 商户通知的队列消费不在本层：worker 侧直接把 service.SendNotify 注册到 pkg/queue
// 的 Worker 上（见 cmd/payment/initial），本层只负责把卡住的任务重新入队。
package job
