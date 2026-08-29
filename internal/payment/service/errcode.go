package service

import (
	"github.com/yanking/go-skeleton/pkg/errcode"
	"google.golang.org/grpc/codes"
)

// payment 业务码（50000–59999 分段，登记于 AGENTS.md）。
var (
	ErrDuplicateOrder     = errcode.New(50001, "商户订单号重复", codes.AlreadyExists)
	ErrAmountOutOfLimit   = errcode.New(50002, "金额超出限额", codes.InvalidArgument)
	ErrChannelNotBound    = errcode.New(50003, "指定渠道未绑定或不可用", codes.FailedPrecondition)
	ErrNoAvailableChannel = errcode.New(50004, "无可用渠道", codes.Unavailable)
	ErrStateConflict      = errcode.New(50005, "订单状态冲突", codes.FailedPrecondition)
	ErrMerchantRestricted = errcode.New(50006, "商户状态受限", codes.PermissionDenied)
)
