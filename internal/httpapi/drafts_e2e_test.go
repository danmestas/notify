package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/agents/wire"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// startDraftServer is startAgentServer plus the agent store and the API key's
// scope, so a test can seed drafts the way the producer will.
func startDraftServer(t *testing.T) (*httptest.Server, string, *agentstore.Store, domain.Scope) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id := identity.New(st, bytes.Repeat([]byte{0x2a}, 32))
	aud := audience.New(st)
	ags := agentstore.New(st)
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil), Inbox: inboxapi.New(st), Agents: agentsapi.New(ags),
	}))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key, ags, scope
}

func seedDraft(t *testing.T, ags *agentstore.Store, scope domain.Scope, subscriber, mailbox, activity string) agentdomain.Draft {
	t.Helper()
	d, err := ags.Drafts().Create(context.Background(), scope, agentdomain.Draft{
		SubscriberID: subscriber, Mailbox: mailbox, AgentIdentifier: "support", AgentName: "Support",
		ConversationID: "th-" + activity, ActivityID: activity, SourceMessageID: "m-" + activity, Text: "reply " + activity,
	})
	if err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	return d
}

func getDrafts(t *testing.T, srv *httptest.Server, auth, query string) (int, wire.DraftList) {
	t.Helper()
	status, body := doGet(t, srv.URL+"/v1/inbox/drafts?"+query, auth)
	var out wire.DraftList
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
	}
	return status, out
}

func draftIDs(l wire.DraftList) []string {
	out := []string{}
	for _, d := range l.Drafts {
		out = append(out, d.ActivityID)
	}
	return out
}

// TestDrafts_PagesByMailbox: a caller sees only the subscriber's drafts in the
// mailboxes it names, oldest first, and pages through them with the cursor
// without skipping or repeating one.
func TestDrafts_PagesByMailbox(t *testing.T) {
	srv, auth, ags, scope := startDraftServer(t)
	seedDraft(t, ags, scope, "owner", "personal", "a1")
	seedDraft(t, ags, scope, "owner", "shared", "a2")
	seedDraft(t, ags, scope, "owner", "lou", "a3") // another holder's mailbox
	seedDraft(t, ags, scope, "other", "personal", "a4")
	seedDraft(t, ags, scope, "owner", "personal", "a5")

	q := "subscriberId=owner&mailbox=personal&mailbox=shared&limit=2"
	status, page1 := getDrafts(t, srv, auth, q)
	if status != http.StatusOK || !equal(draftIDs(page1), []string{"a1", "a2"}) || page1.Gap {
		t.Fatalf("page 1 = %d %v gap=%v", status, draftIDs(page1), page1.Gap)
	}
	_, page2 := getDrafts(t, srv, auth, q+"&after="+page1.Cursor)
	if !equal(draftIDs(page2), []string{"a5"}) {
		t.Fatalf("page 2 = %v, want [a5]", draftIDs(page2))
	}
	_, page3 := getDrafts(t, srv, auth, q+"&after="+page2.Cursor)
	if len(page3.Drafts) != 0 || page3.Cursor != page2.Cursor {
		t.Fatalf("page 3 = %v cursor %q, want empty with the cursor unchanged (%q)", draftIDs(page3), page3.Cursor, page2.Cursor)
	}

	// A draft produced after the caller caught up arrives on the next poll.
	seedDraft(t, ags, scope, "owner", "shared", "a6")
	if _, page4 := getDrafts(t, srv, auth, q+"&after="+page3.Cursor); !equal(draftIDs(page4), []string{"a6"}) {
		t.Fatalf("page 4 = %v, want [a6]", draftIDs(page4))
	}
}

// TestDrafts_GapWhenTheCursorPredatesRetention: a cursor older than the
// retention window is answered with gap, so the caller can say that drafts
// may have expired rather than lose them silently.
func TestDrafts_GapWhenTheCursorPredatesRetention(t *testing.T) {
	srv, auth, ags, scope := startDraftServer(t)
	seedDraft(t, ags, scope, "owner", "personal", "b1")
	old := time.Now().Add(-agentstore.DraftRetention - time.Hour).UnixMilli()
	_, list := getDrafts(t, srv, auth, "subscriberId=owner&mailbox=personal&after=0."+strconv.FormatInt(old, 10))
	if !list.Gap || !equal(draftIDs(list), []string{"b1"}) {
		t.Fatalf("gap=%v drafts=%v, want gap and [b1]", list.Gap, draftIDs(list))
	}
	recent := time.Now().Add(-time.Hour).UnixMilli()
	if _, list := getDrafts(t, srv, auth, "subscriberId=owner&mailbox=personal&after=0."+strconv.FormatInt(recent, 10)); list.Gap {
		t.Fatal("gap for a cursor inside the retention window")
	}
}

func TestDrafts_RequestRules(t *testing.T) {
	srv, auth, _, _ := startDraftServer(t)
	for _, q := range []string{"mailbox=personal", "subscriberId=owner", "subscriberId=owner&mailbox=m&after=nonsense"} {
		if status, _ := getDrafts(t, srv, auth, q); status != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", q, status)
		}
	}
	if status, _ := getDrafts(t, srv, "", "subscriberId=owner&mailbox=m"); status != http.StatusUnauthorized {
		t.Errorf("no API key: status %d, want 401", status)
	}
}

// TestDrafts_CreateIsIdempotentPerActivity: producing the draft for one agent
// reply twice stores it once.
func TestDrafts_CreateIsIdempotentPerActivity(t *testing.T) {
	_, _, ags, scope := startDraftServer(t)
	first := seedDraft(t, ags, scope, "owner", "personal", "c1")
	again := seedDraft(t, ags, scope, "owner", "personal", "c1")
	if again.ID != first.ID || again.Seq != first.Seq {
		t.Fatalf("second create = %+v, want the first (%+v)", again, first)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDrafts_PurgeRemovesOnlyExpired: the retention purge deletes drafts
// created before the cutoff and keeps the rest.
func TestDrafts_PurgeRemovesOnlyExpired(t *testing.T) {
	_, _, ags, scope := startDraftServer(t)
	seedDraft(t, ags, scope, "owner", "personal", "p1")
	ctx := context.Background()
	if n, err := ags.Drafts().PurgeOlderThan(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("purge of nothing expired: %d, %v", n, err)
	}
	if n, err := ags.Drafts().PurgeOlderThan(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("purge: %d, %v; want 1", n, err)
	}
}
