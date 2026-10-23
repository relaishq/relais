package movediag

import (
	"context"
	"strings"

	"github.com/redis/go-redis/v9"
)

type RedisHook struct{}

func (RedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (RedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if ID(ctx) == "" {
			return next(ctx, cmd)
		}
		event := NewEvent(ctx, "redis", strings.ToUpper(cmd.Name()))
		err := next(ctx, cmd)
		Default.Finish(event, err != nil && err != redis.Nil)
		return err
	}
}
func (RedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if ID(ctx) == "" {
			return next(ctx, cmds)
		}
		events := make([]Event, len(cmds))
		for i, cmd := range cmds {
			events[i] = NewEvent(ctx, "redis", strings.ToUpper(cmd.Name()))
		}
		err := next(ctx, cmds)
		for i, event := range events {
			Default.Finish(event, cmds[i].Err() != nil && cmds[i].Err() != redis.Nil)
		}
		return err
	}
}
