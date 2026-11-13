package webtools

import (
	"container/list"
	"sync"
	"time"
)

// pageCache is a size-bounded LRU of fetched pages with a time-to-live, so
// paging through a long document with start_index downloads it once.
type pageCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	maxBytes int
	size     int
	order    *list.List // front is most recent
	items    map[string]*list.Element
	now      func() time.Time
}

type cacheEntry struct {
	key  string
	page *Page
	size int
	exp  time.Time
}

func newPageCache(ttl time.Duration, maxBytes int) *pageCache {
	return &pageCache{ttl: ttl, maxBytes: maxBytes, order: list.New(), items: map[string]*list.Element{}, now: time.Now}
}

func (c *pageCache) get(key string) (*Page, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry)
	if c.now().After(e.exp) {
		c.remove(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return e.page, true
}

func (c *pageCache) put(key string, p *Page) {
	size := len(p.Content) + len(p.Source) + len(key)
	if size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.remove(el)
	}
	el := c.order.PushFront(&cacheEntry{key: key, page: p, size: size, exp: c.now().Add(c.ttl)})
	c.items[key] = el
	c.size += size
	for c.size > c.maxBytes {
		c.remove(c.order.Back())
	}
}

func (c *pageCache) remove(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.order.Remove(el)
	delete(c.items, e.key)
	c.size -= e.size
}

func (c *pageCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
