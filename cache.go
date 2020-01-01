package twitchhook

import (
	"sync"
	"time"
)

// Subscription is a cache item
type Subscription struct {
	Topic          string
	CallbackURL    string
	Lease          time.Duration
	Secret         string
	DenialCallback func(reason string)
	Renew          func()
}

// SubscriptionManager manages subscription state
type SubscriptionManager interface {
	Delete(topic string) error
	Get(topic string) (*Subscription, error)
	Save(topic string, sub *Subscription) error
	SetSubscriptionLease(topic string, lease time.Duration) (exists bool, err error)
}

type cacheItem struct {
	sub   *Subscription
	timer *time.Timer
}

// InMemoryCache caches subscriptions
type InMemoryCache struct {
	c map[string]*cacheItem
	m sync.RWMutex
}

// Get retrieves a subscription
func (c *InMemoryCache) Get(topic string) (*Subscription, error) {
	c.m.RLock()
	defer c.m.RUnlock()

	item := c.c[topic]
	return item.sub, nil
}

// Save caches a subscription, returns an error if a subscription already exists
func (c *InMemoryCache) Save(topic string, sub *Subscription) error {
	c.m.Lock()
	defer c.m.Unlock()

	c.c[topic] = &cacheItem{
		sub:   sub,
		timer: time.AfterFunc(sub.Lease, sub.Renew),
	}
	return nil
}

// SetSubscriptionLease retrieves a subscription
func (c *InMemoryCache) SetSubscriptionLease(topic string, lease time.Duration) (bool, error) {
	c.m.Lock()
	defer c.m.Unlock()

	item, ok := c.c[topic]
	if !ok {
		return false, nil
	}

	item.timer.Stop()
	item.timer = time.AfterFunc(item.sub.Lease, item.sub.Renew)
	item.sub.Lease = lease
	return true, nil
}

// Delete removes a subscription
func (c *InMemoryCache) Delete(topic string) error {
	c.m.Lock()
	defer c.m.Unlock()

	item, ok := c.c[topic]
	if !ok {
		return nil
	}

	item.timer.Stop()

	delete(c.c, topic)
	return nil
}
