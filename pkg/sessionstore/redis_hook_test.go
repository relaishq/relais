package sessionstore

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// Keep fault sequencing independent of the driver's hook API. Embedded fault
// overrides still dispatch on the full hook, before and after the real command.
type redisFaultSequence interface {
	BeforeProcess(context.Context, redis.Cmder) (context.Context, error)
	AfterProcess(context.Context, redis.Cmder) error
	BeforeProcessPipeline(context.Context, []redis.Cmder) (context.Context, error)
	AfterProcessPipeline(context.Context, []redis.Cmder) error
}
type redisFaultHook struct{ redisFaultSequence }

func (redisFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h redisFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		ctx, err := h.BeforeProcess(ctx, cmd)
		if err == nil {
			err = next(ctx, cmd)
		}
		cmd.SetErr(err)
		if after := h.AfterProcess(ctx, cmd); after != nil {
			err = after
			cmd.SetErr(err)
		}
		return err
	}
}
func (h redisFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		setErrors := func(err error) {
			for _, cmd := range cmds {
				if cmd.Err() == nil {
					cmd.SetErr(err)
				}
			}
		}
		ctx, err := h.BeforeProcessPipeline(ctx, cmds)
		if err == nil {
			err = next(ctx, cmds)
		} else {
			setErrors(err)
		}
		if after := h.AfterProcessPipeline(ctx, cmds); after != nil {
			err = after
			setErrors(err)
		}
		return err
	}
}
