package proxy

import (
	"sync"
	"testing"
)

func TestFormStoreConsumesHiddenFieldsAtomically(t *testing.T) {
	store := newFormStore()
	action := "https://example.test/login"
	store.Store("session", map[string]map[string]string{
		action: {"csrf": "one-time"},
	})

	const workers = 2
	results := make(chan bool, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, changed := store.Augment("session", action, "user=alice")
			results <- changed
		}()
	}
	wg.Wait()
	close(results)
	applied := 0
	for changed := range results {
		if changed {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("one-time form state applied %d times, want 1", applied)
	}
}
