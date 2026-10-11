package movediag

import (
	"context"
	"strings"

	"github.com/go-redis/redis/v8"
)

type RedisHook struct{}
type redisKey struct{}

func (RedisHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if ID(ctx) == "" {
		return ctx, nil
	}
	return context.WithValue(ctx, redisKey{}, []Event{NewEvent(ctx, "redis", strings.ToUpper(cmd.Name()))}), nil
}
func (RedisHook) AfterProcess(ctx context.Context, cmd redis.Cmder) error {
	events, _ := ctx.Value(redisKey{}).([]Event)
	if len(events) == 1 {
		Default.Finish(events[0], cmd.Err() != nil && cmd.Err() != redis.Nil)
	}
	return nil
}
func (RedisHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	if ID(ctx) == "" {
		return ctx, nil
	}
	events := make([]Event, len(cmds))
	for i, cmd := range cmds {
		events[i] = NewEvent(ctx, "redis", strings.ToUpper(cmd.Name()))
	}
	return context.WithValue(ctx, redisKey{}, events), nil
}
func (RedisHook) AfterProcessPipeline(ctx context.Context, cmds []redis.Cmder) error {
	events, _ := ctx.Value(redisKey{}).([]Event)
	for i, e := range events {
		Default.Finish(e, cmds[i].Err() != nil && cmds[i].Err() != redis.Nil)
	}
	return nil
}
