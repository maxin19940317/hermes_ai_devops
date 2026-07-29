package feishucmd

import (
	"fmt"
	"testing"
	"time"
)

func TestDedupCache(t *testing.T) {
	now := time.Date(2026, 7, 29, 1, 40, 0, 0, time.UTC)

	t.Run("首次通过,TTL 内重复拒绝", func(t *testing.T) {
		c := newDedupCache(10*time.Minute, 100)
		if !c.addIfNew("m1", now) {
			t.Fatal("首次应通过")
		}
		if c.addIfNew("m1", now.Add(5*time.Minute)) {
			t.Error("TTL 内重复应拒绝")
		}
		if !c.addIfNew("m2", now.Add(5*time.Minute)) {
			t.Error("不同 id 应通过")
		}
	})

	t.Run("TTL 过期后同 id 重新放行", func(t *testing.T) {
		c := newDedupCache(10*time.Minute, 100)
		c.addIfNew("m1", now)
		if !c.addIfNew("m1", now.Add(11*time.Minute)) {
			t.Error("过期后应重新放行")
		}
	})

	t.Run("容量上限淘汰最旧", func(t *testing.T) {
		c := newDedupCache(time.Hour, 3)
		c.addIfNew("m1", now)
		c.addIfNew("m2", now.Add(time.Second))
		c.addIfNew("m3", now.Add(2*time.Second))
		// 已满:加入 m4 时淘汰最旧的 m1
		if !c.addIfNew("m4", now.Add(3*time.Second)) {
			t.Fatal("新 id 应通过")
		}
		if !c.addIfNew("m1", now.Add(4*time.Second)) {
			t.Error("被淘汰的最旧 id 应视为新 id 放行")
		}
		// m1 重新入队时按同样规则淘汰了当前的队首 m2
		if !c.addIfNew("m2", now.Add(5*time.Second)) {
			t.Error("m2 已被 m1 的重新入队淘汰,应放行")
		}
		if c.addIfNew("m4", now.Add(6*time.Second)) {
			t.Error("未被淘汰的 id 仍应拒绝")
		}
	})

	t.Run("容量紧张时先清过期", func(t *testing.T) {
		c := newDedupCache(time.Minute, 2)
		c.addIfNew("old", now)
		c.addIfNew("m1", now.Add(50*time.Second))
		// old 已过期,应清它而不是 m1
		if !c.addIfNew("m2", now.Add(70*time.Second)) {
			t.Fatal("新 id 应通过")
		}
		if c.addIfNew("m1", now.Add(71*time.Second)) {
			t.Error("未过期项不应被误淘汰")
		}
	})

	t.Run("并发安全", func(t *testing.T) {
		c := newDedupCache(time.Hour, 10000)
		done := make(chan struct{})
		for g := 0; g < 8; g++ {
			go func(g int) {
				defer func() { done <- struct{}{} }()
				for i := 0; i < 200; i++ {
					c.addIfNew(fmt.Sprintf("g%d-m%d", g, i), now)
				}
			}(g)
		}
		for g := 0; g < 8; g++ {
			<-done
		}
		if len(c.seen) != 1600 {
			t.Errorf("seen = %d, want 1600", len(c.seen))
		}
	})
}
