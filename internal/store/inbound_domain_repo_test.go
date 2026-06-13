package store

import (
	"context"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestInboundDomainRepo_CRUD(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	d, err := s.InboundDomains().Create(ctx, scope, "example.com", "inbound.notify.example.com", 10)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if d.Name != "example.com" {
		t.Errorf("Name = %q, want example.com", d.Name)
	}
	if d.Status != "pending" {
		t.Errorf("Status = %q, want pending", d.Status)
	}

	got, err := s.InboundDomains().Get(ctx, scope, "example.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != d.ID {
		t.Errorf("Get ID mismatch: %q vs %q", got.ID, d.ID)
	}

	if err := s.InboundDomains().SetStatus(ctx, scope, "example.com", "verified", true); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got2, _ := s.InboundDomains().Get(ctx, scope, "example.com")
	if got2.Status != "verified" {
		t.Errorf("Status after SetStatus = %q, want verified", got2.Status)
	}
	if !got2.MXConfigured {
		t.Error("MXConfigured should be true after SetStatus")
	}

	items, next, total, err := s.InboundDomains().List(ctx, scope, "", "", 20)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(items) != 1 {
		t.Errorf("len(items) = %d, want 1", len(items))
	}
	if next != "" {
		t.Errorf("next = %q, want empty", next)
	}

	if err := s.InboundDomains().Delete(ctx, scope, "example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.InboundDomains().Get(ctx, scope, "example.com"); err != ErrNotFound {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestInboundDomainRepo_DuplicateConflict(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	if _, err := s.InboundDomains().Create(ctx, scope, "dup.com", "mx.example.com", 10); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := s.InboundDomains().Create(ctx, scope, "dup.com", "mx.example.com", 10)
	if err != ErrInboundDomainConflict {
		t.Errorf("second Create = %v, want ErrInboundDomainConflict", err)
	}
}

func TestInboundDomainRepo_ScopeIsolation(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scopeA := domain.Scope{OrgID: "orgA", EnvID: "envA"}
	scopeB := domain.Scope{OrgID: "orgB", EnvID: "envB"}

	if _, err := s.InboundDomains().Create(ctx, scopeA, "a.com", "mx.a.com", 10); err != nil {
		t.Fatalf("Create A: %v", err)
	}

	items, _, _, err := s.InboundDomains().List(ctx, scopeB, "", "", 20)
	if err != nil {
		t.Fatalf("List B: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("scopeB sees %d items from scopeA, want 0", len(items))
	}

	if _, err := s.InboundDomains().Get(ctx, scopeB, "a.com"); err != ErrNotFound {
		t.Errorf("Get from scopeB = %v, want ErrNotFound", err)
	}
}

func TestInboundDomainRepo_KeysetPagination(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}

	names := []string{"a.com", "b.com", "c.com", "d.com", "e.com"}
	for _, n := range names {
		if _, err := s.InboundDomains().Create(ctx, scope, n, "mx.example.com", 10); err != nil {
			t.Fatalf("Create %s: %v", n, err)
		}
	}

	page1, next, total, err := s.InboundDomains().List(ctx, scope, "", "", 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	if page1[0].Name != "a.com" || page1[1].Name != "b.com" {
		t.Errorf("page1 names = %v", []string{page1[0].Name, page1[1].Name})
	}

	page2, next2, _, err := s.InboundDomains().List(ctx, scope, "", next, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}
	if page2[0].Name != "c.com" || page2[1].Name != "d.com" {
		t.Errorf("page2 names = %v", []string{page2[0].Name, page2[1].Name})
	}

	page3, _, _, _ := s.InboundDomains().List(ctx, scope, "", next2, 2)
	if len(page3) != 1 || page3[0].Name != "e.com" {
		t.Errorf("page3 = %v", page3)
	}
}

func TestInboundDomainRepo_DeleteNotFound(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org1", EnvID: "env1"}
	if err := s.InboundDomains().Delete(ctx, scope, "nonexistent.com"); err != ErrNotFound {
		t.Errorf("Delete nonexistent = %v, want ErrNotFound", err)
	}
}
