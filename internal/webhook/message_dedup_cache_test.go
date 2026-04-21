package webhook

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMessageDedupCache_PutThenGet(t *testing.T) {
	c := NewMessageDedupCache(10*time.Minute, 100)

	_, ok := c.Get("proj", "card-1", "msg-1")
	assert.False(t, ok, "empty cache must miss")

	c.Put("proj", "card-1", "msg-1", CachedAck{
		Status: http.StatusAccepted,
		Body:   []byte(`{"ok":true,"message_id":"msg-1"}`),
	})

	ack, ok := c.Get("proj", "card-1", "msg-1")
	assert.True(t, ok)
	assert.Equal(t, http.StatusAccepted, ack.Status)
	assert.JSONEq(t, `{"ok":true,"message_id":"msg-1"}`, string(ack.Body))
}

func TestMessageDedupCache_EmptyMessageIDIsIgnored(t *testing.T) {
	c := NewMessageDedupCache(10*time.Minute, 100)

	c.Put("proj", "card-1", "", CachedAck{Status: 202, Body: []byte("ignored")})
	assert.Equal(t, 0, c.Len(), "empty message_id must not populate cache")

	_, ok := c.Get("proj", "card-1", "")
	assert.False(t, ok, "empty message_id always misses")
}

func TestMessageDedupCache_KeyScoping(t *testing.T) {
	// Same message_id in different (project, card_id) scopes must not collide.
	c := NewMessageDedupCache(10*time.Minute, 100)

	c.Put("alpha", "A-1", "msg-x", CachedAck{Status: 202, Body: []byte("alpha")})
	c.Put("beta", "A-1", "msg-x", CachedAck{Status: 202, Body: []byte("beta")})
	c.Put("alpha", "A-2", "msg-x", CachedAck{Status: 202, Body: []byte("alpha-2")})

	a, ok := c.Get("alpha", "A-1", "msg-x")
	assert.True(t, ok)
	assert.Equal(t, "alpha", string(a.Body))

	b, ok := c.Get("beta", "A-1", "msg-x")
	assert.True(t, ok)
	assert.Equal(t, "beta", string(b.Body))

	a2, ok := c.Get("alpha", "A-2", "msg-x")
	assert.True(t, ok)
	assert.Equal(t, "alpha-2", string(a2.Body))
}

func TestMessageDedupCache_ExpiredAfterTTL(t *testing.T) {
	clock := &mockClock{now: time.Unix(1_700_000_000, 0)}
	c := NewMessageDedupCache(5*time.Minute, 100, WithMessageDedupNow(clock.Now))

	c.Put("proj", "card-1", "msg-1", CachedAck{Status: 202, Body: []byte("body")})

	_, ok := c.Get("proj", "card-1", "msg-1")
	assert.True(t, ok, "hit within TTL")

	clock.advance(6 * time.Minute)

	_, ok = c.Get("proj", "card-1", "msg-1")
	assert.False(t, ok, "miss after TTL expiry")
}

func TestMessageDedupCache_CapacityEviction(t *testing.T) {
	c := NewMessageDedupCache(0, 2)

	c.Put("p", "c", "m1", CachedAck{Status: 202, Body: []byte("one")})
	c.Put("p", "c", "m2", CachedAck{Status: 202, Body: []byte("two")})
	c.Put("p", "c", "m3", CachedAck{Status: 202, Body: []byte("three")})

	assert.Equal(t, 2, c.Len())

	_, ok := c.Get("p", "c", "m1")
	assert.False(t, ok, "oldest entry must be evicted")

	_, ok = c.Get("p", "c", "m2")
	assert.True(t, ok)

	_, ok = c.Get("p", "c", "m3")
	assert.True(t, ok)
}

func TestMessageDedupCache_BodyIsDefensivelyCopied(t *testing.T) {
	c := NewMessageDedupCache(10*time.Minute, 100)

	original := []byte(`{"ok":true}`)
	c.Put("p", "c", "m1", CachedAck{Status: 202, Body: original})

	// Mutate caller's slice.
	original[0] = 'X'

	ack, ok := c.Get("p", "c", "m1")
	assert.True(t, ok)
	assert.Equal(t, `{"ok":true}`, string(ack.Body), "stored body must not alias caller's slice")

	// Mutate returned slice — cache must stay intact.
	ack.Body[0] = 'Y'
	ack2, ok := c.Get("p", "c", "m1")
	assert.True(t, ok)
	assert.Equal(t, `{"ok":true}`, string(ack2.Body), "returned slice must not alias cache storage")
}

func TestMessageDedupCache_Run_StopsOnContextCancel(t *testing.T) {
	c := NewMessageDedupCache(time.Second, 10)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		c.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
