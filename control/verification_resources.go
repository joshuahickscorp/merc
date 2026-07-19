package main

import (
	"context"
	"errors"
	"sync"
)

const (
	verificationDBHeadroom int32 = 2

	verificationArtifactMemoryCeiling int64 = (1 << 30) + (64 << 20) // 1.0625 GiB
)

var (
	ErrVerificationResourceBusy = errors.New("verification resource budget is busy")
	ErrVerificationPoolTooSmall = errors.New("verification requires at least two database connections")
)

type verificationResourceBudget struct {
	processSlots chan struct{}
	bytes        *weightedByteBudget
	maxConns     int32
	reservedDB   int32
}

func newVerificationResourceBudget(maxConns int32, byteCeiling int64) *verificationResourceBudget {
	reserved := verificationDBHeadroom
	if maxConns <= reserved {
		reserved = maxConns - 1
	}
	if reserved < 0 {
		reserved = 0
	}
	processes := maxConns - reserved
	if maxConns < 2 {
		processes = 0
	}
	if byteCeiling <= 0 {
		byteCeiling = verificationArtifactMemoryCeiling
	}
	return &verificationResourceBudget{
		processSlots: make(chan struct{}, int(processes)),
		bytes:        newWeightedByteBudget(byteCeiling),
		maxConns:     maxConns,
		reservedDB:   reserved,
	}
}

func (b *verificationResourceBudget) tryAcquireProcess() bool {
	if b == nil || cap(b.processSlots) == 0 {
		return false
	}
	select {
	case b.processSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (b *verificationResourceBudget) releaseProcess() {
	if b == nil {
		return
	}
	select {
	case <-b.processSlots:
	default:
		panic("verification process permit released without acquisition")
	}
}

type weightedByteBudget struct {
	mu      sync.Mutex
	ceiling int64
	inUse   int64
	peak    int64
}

func newWeightedByteBudget(ceiling int64) *weightedByteBudget {
	if ceiling <= 0 {
		panic("verification byte budget requires a positive ceiling")
	}
	return &weightedByteBudget{ceiling: ceiling}
}

func (b *weightedByteBudget) tryAcquire(n int64) bool {
	if b == nil || n < 0 {
		return false
	}
	if n == 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.ceiling-b.inUse {
		return false
	}
	b.inUse += n
	if b.inUse > b.peak {
		b.peak = b.inUse
	}
	return true
}

func (b *weightedByteBudget) release(n int64) {
	if b == nil || n == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n > b.inUse {
		panic("verification byte budget released beyond acquisition")
	}
	b.inUse -= n
}

type verificationMemoryTrackerKey struct{}

type verificationMemoryTracker struct {
	mu     sync.Mutex
	budget *weightedByteBudget
	held   int64
	bodies map[string][]byte
}

func withVerificationMemoryTracker(ctx context.Context, budget *weightedByteBudget) (context.Context, func()) {
	if budget == nil {
		return ctx, func() {}
	}
	tracker := &verificationMemoryTracker{budget: budget, bodies: make(map[string][]byte)}
	return context.WithValue(ctx, verificationMemoryTrackerKey{}, tracker), tracker.releaseAll
}

func cachedVerificationBody(ctx context.Context, key string) ([]byte, bool) {
	tracker := verificationMemoryTrackerFrom(ctx)
	if tracker == nil {
		return nil, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	body, ok := tracker.bodies[key]
	return body, ok
}

func cacheVerificationBody(ctx context.Context, key string, body []byte) {
	tracker := verificationMemoryTrackerFrom(ctx)
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.bodies[key] = body
}

func verificationMemoryTrackerFrom(ctx context.Context) *verificationMemoryTracker {
	if ctx == nil {
		return nil
	}
	tracker, _ := ctx.Value(verificationMemoryTrackerKey{}).(*verificationMemoryTracker)
	return tracker
}

func reserveVerificationMemory(ctx context.Context, n int64) error {
	tracker := verificationMemoryTrackerFrom(ctx)
	if tracker == nil || n == 0 {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if !tracker.budget.tryAcquire(n) {
		return ErrVerificationResourceBusy
	}
	tracker.held += n
	return nil
}

func verificationMemoryMark(ctx context.Context) int64 {
	tracker := verificationMemoryTrackerFrom(ctx)
	if tracker == nil {
		return 0
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.held
}

func releaseVerificationMemoryToMark(ctx context.Context, mark int64) {
	tracker := verificationMemoryTrackerFrom(ctx)
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if mark < 0 || mark > tracker.held {
		panic("invalid verification memory mark")
	}
	release := tracker.held - mark
	tracker.held = mark
	tracker.bodies = make(map[string][]byte)
	tracker.budget.release(release)
}

func (t *verificationMemoryTracker) releaseAll() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.budget.release(t.held)
	t.held = 0
	t.bodies = nil
}
