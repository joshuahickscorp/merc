package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var taskWake = newWaker()

type waker struct {
	mu sync.Mutex
	ch chan struct{}
}

func newWaker() *waker {
	return &waker{ch: make(chan struct{})}
}

func (w *waker) Wait() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ch
}

func (w *waker) Broadcast() {
	w.mu.Lock()
	old := w.ch
	w.ch = make(chan struct{})
	w.mu.Unlock()
	close(old)
}

func startTaskWakeListener(ctx context.Context, pool *pgxpool.Pool) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := listenOnce(ctx, pool); err != nil && ctx.Err() == nil {
			log.Printf("notify: LISTEN cx_task_available: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = time.Second // a clean run (ctx cancelled) resets backoff; unreachable on real drop
	}
}

func listenOnce(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN cx_task_available"); err != nil {
		return err
	}
	log.Print("notify: listening for cx_task_available")
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		taskWake.Broadcast()
	}
}
