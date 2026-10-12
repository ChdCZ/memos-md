package memofile

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestMirrorGuard builds just the re-entrancy guard of a Mirror, which is
// the part that keeps Sync from deadlocking on its own store writes.
func newTestMirrorGuard() *Mirror {
	mirror := &Mirror{index: newFileIndex()}
	mirror.cond = sync.NewCond(&mirror.mu)
	return mirror
}

func TestEnterIsReentrantOnTheSameGoroutine(t *testing.T) {
	mirror := newTestMirrorGuard()
	require.True(t, mirror.enter(), "the first call acquires the mirror")

	// A nested call from the same pass must not wait for the outer one.
	require.False(t, mirror.enter())

	// Another goroutine waits until the holding goroutine leaves.
	acquired := make(chan bool, 1)
	go func() { acquired <- mirror.enter() }()
	select {
	case <-acquired:
		t.Fatal("a second goroutine must wait for the mirror")
	case <-time.After(50 * time.Millisecond):
	}

	mirror.leave(true)
	select {
	case nested := <-acquired:
		require.True(t, nested, "the waiting goroutine should acquire the mirror")
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting goroutine never acquired the mirror")
	}
}

func TestEnterSerialisesConcurrentCallers(t *testing.T) {
	mirror := newTestMirrorGuard()
	const workers = 8
	var active int
	var peak int
	var mu sync.Mutex
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if !mirror.enter() {
				return
			}
			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			mirror.leave(true)
		}()
	}
	waitGroup.Wait()
	require.Equal(t, 1, peak, "only one goroutine may hold the mirror at a time")
}
