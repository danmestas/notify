package store

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// tenantCase is one row of the tenancy-isolation table: it names a Store repo
// accessor and either supplies a cross-tenant leak probe (check) or records why
// the repo has no scoped read surface to probe (exempt).
//
// The invariant this table enforces: every repo that vends a scoped list/get
// returns ONLY rows owned by the querying (org, environment) scope. Tenancy is
// otherwise a hand-written convention — 179 org_id / 216 environment_id
// predicates hand-repeated across internal/store/*.go — with nothing that fails
// when a new repo or method forgets the predicate. This test is that failure.
type tenantCase struct {
	// name is the exact Store accessor method name (e.g. "Subscribers"). It is
	// matched against the reflected set in TestTenantIsolation_CoversAllRepos so
	// a newly-added repo accessor cannot silently skip this test.
	name string
	// exempt, when non-empty, documents why the repo is intentionally not
	// cross-tenant-probed. Mutually exclusive with check.
	exempt string
	// check seeds one row under scope a and one under scope b, then exercises
	// every scoped list/get as b and asserts a's rows never appear. It must fail
	// the test (via t.Error/Fatalf) on any leak.
	check func(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope)
}

// tenantCases is the authoritative table. TestTenantIsolation_CoversAllRepos
// fails if any *Store repo accessor is missing here, so this list stays
// exhaustive as repos are added.
func tenantCases() []tenantCase {
	return []tenantCase{
		{name: "Subscribers", check: checkSubscribers},
		{name: "SubscriberChannels", check: checkSubscriberChannels},
		{name: "WebPushSubscriptions", check: checkWebPushSubscriptions},
		{name: "Workflows", check: checkWorkflows},
		{name: "WorkflowVersions", check: checkWorkflowVersions},
		{name: "Integrations", check: checkIntegrations},
		{name: "Notifications", check: checkNotifications},
		{name: "Messages", check: checkMessages},
		{name: "Preferences", check: checkPreferences},
		{name: "Analytics", check: checkAnalytics},
		{name: "ApiKeys", check: checkApiKeys},
		{name: "WebhookEndpoints", check: checkWebhookEndpoints},
		{name: "Topics", check: checkTopics},
		{name: "TopicTriggers", check: checkTopicTriggers},
		{name: "Translations", check: checkTranslations},
		{name: "Layouts", check: checkLayouts},
		{name: "Contexts", check: checkContexts},
		{name: "EnvVars", check: checkEnvVars},
		{name: "InboundDomains", check: checkInboundDomains},
		{name: "StreamSources", check: checkStreamSources},
		{name: "EmailThreads", check: checkEmailThreads},
		{name: "ChatThreads", check: checkChatThreads},
		{name: "BuilderSessions", check: checkBuilderSessions},
		{name: "Environments", check: checkEnvironments},

		// Exempt: no scoped read surface to cross-tenant-probe. Recorded so the
		// completeness guard still forces a deliberate decision per repo.
		{
			name: "Organizations",
			exempt: "tenant root — OrganizationRepo exposes only Create(name); it has no " +
				"Scope-typed read to leak-check (an organization IS the tenant, not a row within one).",
		},
		{
			name: "StepOutputs",
			exempt: "its only read (ListByRun) is intentionally unscoped by design — a downstream " +
				"worker holds an opaque run id and no tenancy context (see store.go StepOutputs()); " +
				"Save is write-only, so there is no scoped list/get to cross-tenant-probe.",
		},
		{
			name: "DelayDeadlines",
			exempt: "same shape as StepOutputs: Get/Clear are intentionally unscoped, keyed by the " +
				"already-globally-unique (run_id, step_id) the delay worker holds (see store.go " +
				"DelayDeadlines()); Save carries scope only for the org/env compliance columns, so " +
				"there is no scoped list/get to cross-tenant-probe.",
		},
	}
}

func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	a := domain.Scope{OrgID: "o1", EnvID: "e1"}
	b := domain.Scope{OrgID: "o2", EnvID: "e2"}

	for _, tc := range tenantCases() {
		tc := tc
		if tc.check == nil {
			continue // exempt repo: nothing to probe, documented in the table
		}
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, ctx, s, a, b)
		})
	}
}

// TestTenantIsolation_CoversAllRepos is the completeness guard: it reflects
// over *Store's exported accessors, keeps the ones that return a repo interface
// (a no-argument method whose sole result type name ends in "Repo"), and fails
// if any of them is absent from tenantCases. A new repo cannot slip past the
// isolation table by omission — adding the accessor turns this test red until
// the author supplies a check or a documented exemption.
func TestTenantIsolation_CoversAllRepos(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range tenantCases() {
		if tc.check == nil && tc.exempt == "" {
			t.Errorf("table entry %q has neither a check nor an exempt reason", tc.name)
		}
		if covered[tc.name] {
			t.Errorf("duplicate table entry %q", tc.name)
		}
		covered[tc.name] = true
	}

	var missing []string
	st := reflect.TypeOf((*Store)(nil))
	for i := 0; i < st.NumMethod(); i++ {
		m := st.Method(i)
		// Repo accessors take only the receiver and return exactly one value.
		if m.Type.NumIn() != 1 || m.Type.NumOut() != 1 {
			continue
		}
		out := m.Type.Out(0)
		if out.Kind() != reflect.Interface || !strings.HasSuffix(out.Name(), "Repo") {
			continue
		}
		if !covered[m.Name] {
			missing = append(missing, m.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d repo accessor(s) missing a tenancy-isolation table entry: %v\n"+
			"add a check (cross-tenant probe) or an exempt reason to tenantCases()", len(missing), missing)
	}
}

// --- helpers ---------------------------------------------------------------

func mustNotFound(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("%s: expected ErrNotFound cross-tenant, got %v", what, err)
	}
}

func forbidID(t *testing.T, what, forbidden string, ids []string) {
	t.Helper()
	for _, id := range ids {
		if id == forbidden {
			t.Errorf("%s: cross-tenant leak — scope-A id %q appeared in scope-B result %v", what, forbidden, ids)
			return
		}
	}
}

func requireID(t *testing.T, what, want string, ids []string) {
	t.Helper()
	for _, id := range ids {
		if id == want {
			return
		}
	}
	t.Errorf("%s: own-scope id %q missing from result %v (query is over-filtering, not isolating)", what, want, ids)
}

// --- per-repo checks -------------------------------------------------------

func checkSubscribers(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Subscribers()
	subA, err := repo.Upsert(ctx, a, domain.Subscriber{SubscriberID: "sub-A", Email: "a@x.test"})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	subB, err := repo.Upsert(ctx, b, domain.Subscriber{SubscriberID: "sub-B", Email: "b@x.test"})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	page, err := repo.List(ctx, b, "", 0)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var ids []string
	for _, it := range page.Items {
		ids = append(ids, it.ID)
	}
	forbidID(t, "Subscribers.List", subA.ID, ids)
	requireID(t, "Subscribers.List", subB.ID, ids)

	_, err = repo.GetByExternalID(ctx, b, "sub-A")
	mustNotFound(t, "Subscribers.GetByExternalID", err)
	_, err = repo.GetByID(ctx, b, subA.ID)
	mustNotFound(t, "Subscribers.GetByID", err)
}

func checkWorkflows(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Workflows()
	wfA, err := repo.Create(ctx, a, domain.Workflow{Name: "W-A", TriggerIdentifier: "wf-a"})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	wfB, err := repo.Create(ctx, b, domain.Workflow{Name: "W-B", TriggerIdentifier: "wf-b"})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.List(ctx, b)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var ids []string
	for _, w := range list {
		ids = append(ids, w.ID)
	}
	forbidID(t, "Workflows.List", wfA.ID, ids)
	requireID(t, "Workflows.List", wfB.ID, ids)

	_, err = repo.GetByID(ctx, b, wfA.ID)
	mustNotFound(t, "Workflows.GetByID", err)
	_, err = repo.GetByTriggerIdentifier(ctx, b, "wf-a")
	mustNotFound(t, "Workflows.GetByTriggerIdentifier", err)
}

func checkIntegrations(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Integrations()
	intA, err := repo.Create(ctx, a, domain.Integration{ProviderID: "p", Channel: "email", Identifier: "int-a", Active: true})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	intB, err := repo.Create(ctx, b, domain.Integration{ProviderID: "p", Channel: "email", Identifier: "int-b", Active: true})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.List(ctx, b)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var ids []string
	for _, in := range list {
		ids = append(ids, in.ID)
	}
	forbidID(t, "Integrations.List", intA.ID, ids)
	requireID(t, "Integrations.List", intB.ID, ids)

	_, err = repo.GetByID(ctx, b, intA.ID)
	mustNotFound(t, "Integrations.GetByID", err)

	active, err := repo.GetActiveByChannel(ctx, b, "email")
	if err != nil {
		t.Fatalf("GetActiveByChannel(b): %v", err)
	}
	if active.ID == intA.ID {
		t.Errorf("Integrations.GetActiveByChannel: cross-tenant leak — returned scope-A integration %q", intA.ID)
	}
}

func checkWebPushSubscriptions(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.WebPushSubscriptions()
	subA, err := repo.UpsertByEndpoint(ctx, a, domain.WebPushSubscription{
		SubscriberID: "sub", Endpoint: "https://push.example.com/device-a", P256dh: "p256dh-a", Auth: "auth-a",
	})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	subB, err := repo.UpsertByEndpoint(ctx, b, domain.WebPushSubscription{
		SubscriberID: "sub", Endpoint: "https://push.example.com/device-b", P256dh: "p256dh-b", Auth: "auth-b",
	})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.ListBySubscriber(ctx, b, "sub")
	if err != nil {
		t.Fatalf("ListBySubscriber(b): %v", err)
	}
	var ids []string
	for _, sub := range list {
		ids = append(ids, sub.ID)
	}
	forbidID(t, "WebPushSubscriptions.ListBySubscriber", subA.ID, ids)
	requireID(t, "WebPushSubscriptions.ListBySubscriber", subB.ID, ids)

	if err := repo.DeleteByEndpoint(ctx, b, "sub", subA.Endpoint); !errors.Is(err, ErrNotFound) {
		t.Errorf("WebPushSubscriptions.DeleteByEndpoint: cross-tenant leak — scope B deleted scope-A endpoint (err=%v, want ErrNotFound)", err)
	}
}

func checkSubscriberChannels(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.SubscriberChannels()
	scA, err := repo.Upsert(ctx, a, domain.SubscriberChannel{
		SubscriberID: "sub", ProviderID: "ntfy", Type: "push", Credentials: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	scB, err := repo.Upsert(ctx, b, domain.SubscriberChannel{
		SubscriberID: "sub", ProviderID: "ntfy", Type: "push", Credentials: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.ListBySubscriber(ctx, b, "sub")
	if err != nil {
		t.Fatalf("ListBySubscriber(b): %v", err)
	}
	var ids []string
	for _, ch := range list {
		ids = append(ids, ch.ID)
	}
	forbidID(t, "SubscriberChannels.ListBySubscriber", scA.ID, ids)
	requireID(t, "SubscriberChannels.ListBySubscriber", scB.ID, ids)

	got, err := repo.GetByProvider(ctx, b, "sub", "ntfy")
	if err != nil {
		t.Fatalf("GetByProvider(b): %v", err)
	}
	if got.ID == scA.ID {
		t.Errorf("SubscriberChannels.GetByProvider: cross-tenant leak — returned scope-A channel %q", scA.ID)
	}
}

func checkWorkflowVersions(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	wfRepo := s.Workflows()
	// workflow_versions rows FK-reference workflows(id); seed a parent per scope.
	wfA, err := wfRepo.Create(ctx, a, domain.Workflow{Name: "WV-A", TriggerIdentifier: "wv-a"})
	if err != nil {
		t.Fatalf("seed parent workflow A: %v", err)
	}
	wfB, err := wfRepo.Create(ctx, b, domain.Workflow{Name: "WV-B", TriggerIdentifier: "wv-b"})
	if err != nil {
		t.Fatalf("seed parent workflow B: %v", err)
	}

	repo := s.WorkflowVersions()
	verA, err := repo.Create(ctx, a, wfA.ID, "WV-A", "wv-a", []byte("{}"), "", "", false)
	if err != nil {
		t.Fatalf("seed version A: %v", err)
	}
	verB, err := repo.Create(ctx, b, wfB.ID, "WV-B", "wv-b", []byte("{}"), "", "", false)
	if err != nil {
		t.Fatalf("seed version B: %v", err)
	}

	// Listing scope-A's workflow id from scope B must return nothing: the
	// scope-checked workflow_id -> workflows join blocks the cross-tenant read.
	crossList, err := repo.ListByWorkflow(ctx, b, wfA.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow(b, wfA): %v", err)
	}
	if len(crossList) != 0 {
		t.Errorf("WorkflowVersions.ListByWorkflow: cross-tenant leak — scope-B saw %d version(s) of scope-A workflow", len(crossList))
	}

	ownList, err := repo.ListByWorkflow(ctx, b, wfB.ID)
	if err != nil {
		t.Fatalf("ListByWorkflow(b, wfB): %v", err)
	}
	var ids []string
	for _, v := range ownList {
		ids = append(ids, v.ID)
	}
	requireID(t, "WorkflowVersions.ListByWorkflow", verB.ID, ids)

	_, err = repo.Get(ctx, b, verA.ID)
	mustNotFound(t, "WorkflowVersions.Get", err)
}

func checkNotifications(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Notifications()
	nA, err := repo.Create(ctx, a, domain.Notification{
		WorkflowID: "w", SubscriberID: "s", TransactionID: "tx-a", Payload: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	nB, err := repo.Create(ctx, b, domain.Notification{
		WorkflowID: "w", SubscriberID: "s", TransactionID: "tx-b", Payload: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	page, err := repo.List(ctx, b, NotificationFilter{}, "", 0)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var ids []string
	for _, it := range page.Items {
		ids = append(ids, it.ID)
	}
	forbidID(t, "Notifications.List", nA.ID, ids)
	requireID(t, "Notifications.List", nB.ID, ids)

	_, err = repo.Get(ctx, b, nA.ID)
	mustNotFound(t, "Notifications.Get", err)
}

func checkMessages(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Messages()
	mA, err := repo.Create(ctx, a, domain.Message{
		SubscriberID: "sub", Channel: "email", Content: "a", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	mB, err := repo.Create(ctx, b, domain.Message{
		SubscriberID: "sub", Channel: "email", Content: "b", Status: "sent",
	})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.List(ctx, b, "sub", "email")
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var ids []string
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	forbidID(t, "Messages.List", mA.ID, ids)
	requireID(t, "Messages.List", mB.ID, ids)
}

func checkPreferences(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Preferences()
	pA, err := repo.Upsert(ctx, a, domain.Preference{
		Type: "subscriberGlobal", SubscriberID: "sub", Channels: []byte(`{"email":true}`),
	})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	pB, err := repo.Upsert(ctx, b, domain.Preference{
		Type: "subscriberGlobal", SubscriberID: "sub", Channels: []byte(`{"email":true}`),
	})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.ListAllForSubscriber(ctx, b, "sub")
	if err != nil {
		t.Fatalf("ListAllForSubscriber(b): %v", err)
	}
	var ids []string
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	forbidID(t, "Preferences.ListAllForSubscriber", pA.ID, ids)
	requireID(t, "Preferences.ListAllForSubscriber", pB.ID, ids)

	got, err := repo.GetGlobal(ctx, b, "sub")
	if err != nil {
		t.Fatalf("GetGlobal(b): %v", err)
	}
	if got.ID == pA.ID {
		t.Errorf("Preferences.GetGlobal: cross-tenant leak — returned scope-A preference %q", pA.ID)
	}
}

func checkAnalytics(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Analytics()
	// Distinct channels per scope: scope-B's aggregate must never surface A's.
	if err := repo.Insert(ctx, a, domain.AnalyticsDelivery{
		Channel: "email", Status: "sent", SubscriberID: "s", WorkflowID: "w", TransactionID: "tx-a",
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := repo.Insert(ctx, b, domain.AnalyticsDelivery{
		Channel: "sms", Status: "sent", SubscriberID: "s", WorkflowID: "w", TransactionID: "tx-b",
	}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	counts, err := repo.CountByChannelStatus(ctx, b, AnalyticsFilter{})
	if err != nil {
		t.Fatalf("CountByChannelStatus(b): %v", err)
	}
	if _, leaked := counts["email"]; leaked {
		t.Errorf("Analytics.CountByChannelStatus: cross-tenant leak — scope-A channel %q counted for scope B: %v", "email", counts)
	}
	if counts["sms"]["sent"] != 1 {
		t.Errorf("Analytics.CountByChannelStatus: own-scope row missing — want sms/sent=1, got %v", counts)
	}
}

func checkApiKeys(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.ApiKeys()
	// GetByHash is credential-keyed (resolves its own scope), so List is the
	// scoped read surface under test here.
	if _, err := repo.Create(ctx, a, domain.APIKey{Hash: "hash-a", EncKey: []byte("k"), UserID: "u"}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := repo.Create(ctx, b, domain.APIKey{Hash: "hash-b", EncKey: []byte("k"), UserID: "u"}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.List(ctx, b)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var hashes []string
	for _, k := range list {
		hashes = append(hashes, k.Hash)
	}
	forbidID(t, "ApiKeys.List", "hash-a", hashes)
	requireID(t, "ApiKeys.List", "hash-b", hashes)
}

func checkWebhookEndpoints(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.WebhookEndpoints()
	weA, err := repo.Create(ctx, a, domain.WebhookEndpoint{URL: "https://a.test", Active: true})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	weB, err := repo.Create(ctx, b, domain.WebhookEndpoint{URL: "https://b.test", Active: true})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.List(ctx, b)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var ids []string
	for _, e := range list {
		ids = append(ids, e.ID)
	}
	forbidID(t, "WebhookEndpoints.List", weA.ID, ids)
	requireID(t, "WebhookEndpoints.List", weB.ID, ids)

	_, err = repo.GetByID(ctx, b, weA.ID)
	mustNotFound(t, "WebhookEndpoints.GetByID", err)
}

func checkTopics(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Topics()
	if _, err := repo.Create(ctx, a, "topic-a", "A"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := repo.Create(ctx, b, "topic-b", "B"); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.List(ctx, b)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var keys []string
	for _, tp := range list {
		keys = append(keys, tp.Key)
	}
	forbidID(t, "Topics.List", "topic-a", keys)
	requireID(t, "Topics.List", "topic-b", keys)

	_, err = repo.GetByKey(ctx, b, "topic-a")
	mustNotFound(t, "Topics.GetByKey", err)
}

func checkTopicTriggers(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.TopicTriggers()
	if err := repo.Record(ctx, a, "tx-a", "topic-x"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := repo.Record(ctx, b, "tx-b", "topic-x"); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	page, err := repo.ListByTopic(ctx, b, "topic-x", "", 0)
	if err != nil {
		t.Fatalf("ListByTopic(b): %v", err)
	}
	var txns []string
	for _, row := range page.Items {
		txns = append(txns, row.TransactionID)
	}
	forbidID(t, "TopicTriggers.ListByTopic", "tx-a", txns)
	requireID(t, "TopicTriggers.ListByTopic", "tx-b", txns)
}

func checkTranslations(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Translations()
	if err := repo.Upsert(ctx, a, "en", "greeting", "hi"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := repo.Upsert(ctx, b, "fr", "greeting", "bonjour"); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	locales, err := repo.ListLocales(ctx, b)
	if err != nil {
		t.Fatalf("ListLocales(b): %v", err)
	}
	forbidID(t, "Translations.ListLocales", "en", locales)
	requireID(t, "Translations.ListLocales", "fr", locales)

	m, err := repo.Map(ctx, b, "en")
	if err != nil {
		t.Fatalf("Map(b, en): %v", err)
	}
	if len(m) != 0 {
		t.Errorf("Translations.Map: cross-tenant leak — scope B saw scope-A locale keys %v", m)
	}
}

func checkLayouts(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Layouts()
	if _, err := repo.Create(ctx, a, "lay-a", "A", "<a/>"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := repo.Create(ctx, b, "lay-b", "B", "<b/>"); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, _, err := repo.List(ctx, b, "", 0)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var keys []string
	for _, l := range list {
		keys = append(keys, l.Key)
	}
	forbidID(t, "Layouts.List", "lay-a", keys)
	requireID(t, "Layouts.List", "lay-b", keys)

	_, err = repo.Get(ctx, b, "lay-a")
	mustNotFound(t, "Layouts.Get", err)
}

func checkContexts(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Contexts()
	cA, err := repo.Create(ctx, a, "user", "ext-a", []byte("{}"))
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := repo.Create(ctx, b, "user", "ext-b", []byte("{}")); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	items, _, _, err := repo.List(ctx, b, "", "", "", "", 0)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var externals []string
	for _, c := range items {
		externals = append(externals, c.ExternalID)
	}
	forbidID(t, "Contexts.List", "ext-a", externals)
	requireID(t, "Contexts.List", "ext-b", externals)

	_, err = repo.Get(ctx, b, "user", "ext-a")
	mustNotFound(t, "Contexts.Get", err)
	_, err = repo.GetByID(ctx, b, cA.ID)
	mustNotFound(t, "Contexts.GetByID", err)
}

func checkEnvVars(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.EnvVars()
	if _, err := repo.Upsert(ctx, a, "KEY_A", false, []byte("enc")); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := repo.Upsert(ctx, b, "KEY_B", false, []byte("enc")); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	items, _, _, err := repo.List(ctx, b, "", "", "", 0)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var keys []string
	for _, v := range items {
		keys = append(keys, v.Key)
	}
	forbidID(t, "EnvVars.List", "KEY_A", keys)
	requireID(t, "EnvVars.List", "KEY_B", keys)

	_, err = repo.Get(ctx, b, "KEY_A")
	mustNotFound(t, "EnvVars.Get", err)
}

func checkInboundDomains(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.InboundDomains()
	if _, err := repo.Create(ctx, a, "a.example", "mx.a", 10); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := repo.Create(ctx, b, "b.example", "mx.b", 10); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	items, _, _, err := repo.List(ctx, b, "", "", 0)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var names []string
	for _, d := range items {
		names = append(names, d.Name)
	}
	forbidID(t, "InboundDomains.List", "a.example", names)
	requireID(t, "InboundDomains.List", "b.example", names)

	_, err = repo.Get(ctx, b, "a.example")
	mustNotFound(t, "InboundDomains.Get", err)
}

func checkStreamSources(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.StreamSources()
	// GetBySecretHash is credential-keyed (resolves its own scope); List and
	// GetByID are the scoped read surface under test.
	ssA, err := repo.Create(ctx, a, domain.StreamSource{
		Name: "S-A", Topic: "alerts", SecretHash: "hash-a", Active: true,
	})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	ssB, err := repo.Create(ctx, b, domain.StreamSource{
		Name: "S-B", Topic: "alerts", SecretHash: "hash-b", Active: true,
	})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.List(ctx, b)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var ids []string
	for _, src := range list {
		ids = append(ids, src.ID)
	}
	forbidID(t, "StreamSources.List", ssA.ID, ids)
	requireID(t, "StreamSources.List", ssB.ID, ids)

	_, err = repo.GetByID(ctx, b, ssA.ID)
	mustNotFound(t, "StreamSources.GetByID", err)
}

func checkEmailThreads(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.EmailThreads()
	etA, err := repo.Upsert(ctx, a, EmailThread{
		PlatformThreadID: "pt-a", SubscriberEmail: "a@x.test", InboundDomain: "a.example",
	})
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	etB, err := repo.Upsert(ctx, b, EmailThread{
		PlatformThreadID: "pt-b", SubscriberEmail: "b@x.test", InboundDomain: "b.example",
	})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	items, _, _, err := repo.List(ctx, b, "", 0)
	if err != nil {
		t.Fatalf("List(b): %v", err)
	}
	var threads []string
	for _, et := range items {
		threads = append(threads, et.PlatformThreadID)
	}
	forbidID(t, "EmailThreads.List", etA.PlatformThreadID, threads)
	requireID(t, "EmailThreads.List", etB.PlatformThreadID, threads)

	_, err = repo.Get(ctx, b, "pt-a")
	mustNotFound(t, "EmailThreads.Get", err)
}

func checkChatThreads(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.ChatThreads()
	if _, err := repo.Upsert(ctx, a, ChatThread{
		Provider: "telegram", ChatID: "c-a", PlatformThreadID: "pt-a", SubscriberID: "s",
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	ctB, err := repo.Upsert(ctx, b, ChatThread{
		Provider: "telegram", ChatID: "c-b", PlatformThreadID: "pt-b", SubscriberID: "s",
	})
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	// ChatThreads has no list; both reads are single-get, checked cross-tenant.
	_, err = repo.GetByChat(ctx, b, "telegram", "c-a")
	mustNotFound(t, "ChatThreads.GetByChat", err)
	_, err = repo.GetByThread(ctx, b, "pt-a")
	mustNotFound(t, "ChatThreads.GetByThread", err)

	own, err := repo.GetByChat(ctx, b, "telegram", "c-b")
	if err != nil {
		t.Fatalf("GetByChat(b, own): %v", err)
	}
	if own.ID != ctB.ID {
		t.Errorf("ChatThreads.GetByChat: own-scope read returned %q, want %q", own.ID, ctB.ID)
	}
}

func checkBuilderSessions(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.BuilderSessions()
	bsA, err := repo.Create(ctx, a, "prompt-a")
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	bsB, err := repo.Create(ctx, b, "prompt-b")
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}

	// BuilderSessions vends per-id reads only (Get); check it cross-tenant.
	_, err = repo.Get(ctx, b, bsA.ID)
	mustNotFound(t, "BuilderSessions.Get", err)

	own, err := repo.Get(ctx, b, bsB.ID)
	if err != nil {
		t.Fatalf("Get(b, own): %v", err)
	}
	if own.ID != bsB.ID {
		t.Errorf("BuilderSessions.Get: own-scope read returned %q, want %q", own.ID, bsB.ID)
	}
}

func checkEnvironments(t *testing.T, ctx context.Context, s *Store, a, b domain.Scope) {
	repo := s.Environments()
	// Environments are scoped by org (an environment IS the env dimension of a
	// scope), so cross-tenant here means cross-org: seed under each scope's org
	// and prove org B never sees org A's environment.
	if _, _, err := repo.Create(ctx, a.OrgID, domain.Environment{ID: "env-a", Name: "A", Identifier: "a"}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, _, err := repo.Create(ctx, b.OrgID, domain.Environment{ID: "env-b", Name: "B", Identifier: "b"}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	list, err := repo.List(ctx, b.OrgID)
	if err != nil {
		t.Fatalf("List(b.Org): %v", err)
	}
	var ids []string
	for _, e := range list {
		ids = append(ids, e.ID)
	}
	forbidID(t, "Environments.List", "env-a", ids)
	requireID(t, "Environments.List", "env-b", ids)

	_, err = repo.Get(ctx, domain.Scope{OrgID: b.OrgID, EnvID: "env-a"})
	mustNotFound(t, "Environments.Get", err)
}
