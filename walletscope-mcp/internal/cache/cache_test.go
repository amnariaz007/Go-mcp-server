package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestExpiration(t *testing.T) {
	now := time.Unix(0, 0)
	c := New[int](time.Minute, 10)
	c.now = func() time.Time { return now }
	c.Set("a", 1)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatal("expected hit")
	}
	now = now.Add(time.Minute)
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry must expire at TTL")
	}
	if c.Len() != 0 {
		t.Fatal("expired entry must be purged on access")
	}
}

func TestLRUEviction(t *testing.T) {
	c := New[int](time.Hour, 2)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("a") // a is now most recently used
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("least recently used entry should be evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("recently used entry evicted")
	}
	if c.Len() != 2 {
		t.Fatalf("len = %d", c.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New[int](time.Hour, 50)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				k := fmt.Sprint(i % 80)
				c.Set(k, i)
				c.Get(k)
			}
		}()
	}
	wg.Wait()
	if c.Len() > 50 {
		t.Fatalf("bound exceeded: %d", c.Len())
	}
}
