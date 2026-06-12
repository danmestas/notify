package store

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestWriterPool_SerialisesWrites(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	repo := s.Subscribers()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	const n = 50
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := repo.Upsert(ctx, scope, domain.Subscriber{
				SubscriberID: fmt.Sprintf("sub-%d", i),
				Email:        fmt.Sprintf("u%d@example.com", i),
			})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("expected no write errors (no SQLITE_BUSY), got %d: %v", len(errs), errs[0])
	}

	page, err := repo.List(ctx, scope, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.TotalCount != n {
		t.Fatalf("expected %d committed subscribers, got %d", n, page.TotalCount)
	}
}
