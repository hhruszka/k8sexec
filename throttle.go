package k8sexec

import (
	"sync"
	"time"
)

type TokenBucket struct {
	tokens chan struct{}
	mu     sync.Mutex
}

func NewTokenBucket(rate int, burst int) *TokenBucket {
	bucket := &TokenBucket{
		tokens: make(chan struct{}, burst),
	}

	// Fill bucket with burst size initially
	for i := 0; i < burst; i++ {
		bucket.tokens <- struct{}{}
	}

	// Refill at given rate
	go func() {
		ticker := time.NewTicker(time.Second / time.Duration(rate))
		defer ticker.Stop()

		for range ticker.C {
			bucket.mu.Lock()
			if len(bucket.tokens) < cap(bucket.tokens) {
				bucket.tokens <- struct{}{}
			}
			bucket.mu.Unlock()
		}
	}()

	return bucket
}

func (tb *TokenBucket) Wait() {
	<-tb.tokens
}
