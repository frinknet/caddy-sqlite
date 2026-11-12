package cache

import (
    "container/list"
    "sync"
    "time"
)

type entry struct {
    key  uint64
    val  any
    exp  time.Time
    elem *list.Element
}

type LRU struct {
    mu  sync.Mutex
    m   map[uint64]*entry
    l   *list.List
    max int
}

func New(max int) *LRU {
    if max <= 0 {
        max = 1000
    }
    return &LRU{m: make(map[uint64]*entry, max), l: list.New(), max: max}
}

func (c *LRU) Get(key uint64) (any, bool) {
    c.mu.Lock()
    defer c.mu.Unlock()
    e, ok := c.m[key]
    if !ok {
        return nil, false
    }
    if time.Now().After(e.exp) {
        c.remove(e)
        return nil, false
    }
    c.l.MoveToFront(e.elem)
    return e.val, true
}

func (c *LRU) Put(key uint64, val any, ttl time.Duration) {
    c.mu.Lock()
    defer c.mu.Unlock()
    if e, ok := c.m[key]; ok {
        e.val = val
        e.exp = time.Now().Add(ttl)
        c.l.MoveToFront(e.elem)
        return
    }
    e := &entry{key: key, val: val, exp: time.Now().Add(ttl)}
    e.elem = c.l.PushFront(e)
    c.m[key] = e
    for len(c.m) > c.max {
        back := c.l.Back()
        if back == nil {
            break
        }
        c.remove(back.Value.(*entry))
    }
}

func (c *LRU) remove(e *entry) {
    delete(c.m, e.key)
    c.l.Remove(e.elem)
}
