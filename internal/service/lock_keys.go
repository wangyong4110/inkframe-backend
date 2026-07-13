package service

import "fmt"

// lockKey 统一构造 Redis 分布式锁 key，格式：lock:<part1>:<part2>:...
// 各调用点原先各自用 fmt.Sprintf 手写拼接，前缀风格不统一，容易因拼写漂移导致锁失效。
func lockKey(parts ...interface{}) string {
	s := "lock"
	for _, p := range parts {
		s += fmt.Sprintf(":%v", p)
	}
	return s
}
