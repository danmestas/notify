package connectivity

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	webpushprovider "github.com/danmestas/notify/internal/providers/webpush"
)

func TestEnsureWebPushKeys_MintsThenReuses(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	pub1, priv1, err := svc.EnsureWebPushKeys(ctx, scope)
	if err != nil {
		t.Fatalf("EnsureWebPushKeys (first call): %v", err)
	}
	if pub1 == "" || priv1 == "" {
		t.Fatalf("first call minted empty keys: pub=%q priv=%q", pub1, priv1)
	}

	pub2, priv2, err := svc.EnsureWebPushKeys(ctx, scope)
	if err != nil {
		t.Fatalf("EnsureWebPushKeys (second call): %v", err)
	}
	if pub2 != pub1 || priv2 != priv1 {
		t.Errorf("second call minted new keys: got (%q,%q), want (%q,%q)", pub2, priv2, pub1, priv1)
	}
}

// TestEnsureWebPushKeys_ConcurrentFirstUse proves the first-use minting race
// is resolved: N concurrent calls on a fresh scope must all succeed with the
// identical keypair, and exactly one integration row must exist afterwards.
// Before the fix, the loser(s) of the race hit the store's UNIQUE
// (environment_id, identifier) constraint on Create and returned a raw error.
func TestEnsureWebPushKeys_ConcurrentFirstUse(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	const n = 8
	type result struct {
		pub, priv string
		err       error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			pub, priv, err := svc.EnsureWebPushKeys(ctx, scope)
			results[i] = result{pub: pub, priv: priv, err: err}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("call %d: EnsureWebPushKeys: %v", i, r.err)
		}
		if r.pub != results[0].pub || r.priv != results[0].priv {
			t.Errorf("call %d returned (%q,%q), want the same keypair as call 0 (%q,%q)", i, r.pub, r.priv, results[0].pub, results[0].priv)
		}
	}

	list, err := svc.store.Integrations().List(ctx, scope)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var webpushRows int
	for _, in := range list {
		if in.ProviderID == webPushProviderID {
			webpushRows++
		}
	}
	if webpushRows != 1 {
		t.Errorf("webpush integration rows = %d, want 1", webpushRows)
	}
}

func TestEnsureWebPushKeys_OtherScopeGetsDifferentKeys(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	// Distinct EnvIDs: environments are globally-unique ULIDs in real usage
	// (integrations.identifier is UNIQUE per environment_id, not per org), so
	// two orgs never actually share one — mirroring that here.
	scopeA := domain.Scope{OrgID: "org-a", EnvID: "e1"}
	scopeB := domain.Scope{OrgID: "org-b", EnvID: "e2"}

	pubA, privA, err := svc.EnsureWebPushKeys(ctx, scopeA)
	if err != nil {
		t.Fatalf("EnsureWebPushKeys scope A: %v", err)
	}
	pubB, privB, err := svc.EnsureWebPushKeys(ctx, scopeB)
	if err != nil {
		t.Fatalf("EnsureWebPushKeys scope B: %v", err)
	}
	if pubA == pubB || privA == privB {
		t.Errorf("different scopes minted the same keypair: A=(%q,%q) B=(%q,%q)", pubA, privA, pubB, privB)
	}
}

// EnsureWebPushKeys mints its row Active:true, same as every other provider's
// Create*. It must still not silently steal an org's existing push provider:
// GetActiveByChannel tie-breaks active rows by primary_provider desc, so a
// push provider explicitly created primary (e.g. ntfy here) keeps resolving
// ahead of a webpush row minted afterwards.
func TestEnsureWebPushKeys_DoesNotShadowAnExistingPrimaryProvider(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, err := svc.CreateNtfy(ctx, scope, "primary-ntfy", NtfyConfig{BaseURL: "http://localhost:2586", Token: "tk"}, true); err != nil {
		t.Fatalf("CreateNtfy: %v", err)
	}
	if _, _, err := svc.EnsureWebPushKeys(ctx, scope); err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}

	_, providerID, _, err := svc.ResolvePush(ctx, scope)
	if err != nil {
		t.Fatalf("ResolvePush: %v", err)
	}
	if providerID != "ntfy" {
		t.Errorf("ResolvePush providerID = %q, want %q (webpush integration must not shadow the primary provider)", providerID, "ntfy")
	}
}

// TestEnsureWebPushKeys_BecomesTheResolvedPushProviderWhenItIsTheOnlyOne
// proves the real end-to-end reachability path: a fresh env with no other
// push integration configured resolves straight to "webpush" once its VAPID
// keypair is minted — no manual PATCH .../active toggle required.
func TestEnsureWebPushKeys_BecomesTheResolvedPushProviderWhenItIsTheOnlyOne(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, _, err := svc.EnsureWebPushKeys(ctx, scope); err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}

	_, providerID, integrationID, err := svc.ResolvePush(ctx, scope)
	if err != nil {
		t.Fatalf("ResolvePush: %v", err)
	}
	if providerID != webPushProviderID {
		t.Errorf("ResolvePush providerID = %q, want %q", providerID, webPushProviderID)
	}
	if integrationID == "" {
		t.Error("ResolvePush integrationID is empty, want the minted row's id")
	}
}

// TestPushProviderFromIntegration_Webpush proves the "webpush" case decrypts
// the minted VAPID keypair and builds a *webpushprovider.Client carrying it —
// the plumbing ResolvePush would use if a webpush integration were ever
// resolved as the scope's active push provider.
func TestPushProviderFromIntegration_Webpush(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	pub, priv, err := svc.EnsureWebPushKeys(ctx, scope)
	if err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}
	in, err := svc.store.Integrations().GetByProviderID(ctx, scope, "webpush")
	if err != nil {
		t.Fatalf("GetByProviderID: %v", err)
	}

	prov, err := svc.PushProviderFromIntegration(ctx, in)
	if err != nil {
		t.Fatalf("PushProviderFromIntegration: %v", err)
	}
	client, ok := prov.(*webpushprovider.Client)
	if !ok {
		t.Fatalf("PushProviderFromIntegration returned %T, want *webpushprovider.Client", prov)
	}
	if client.PublicKey != pub || client.PrivateKey != priv {
		t.Errorf("client keys = (%q,%q), want the minted (%q,%q)", client.PublicKey, client.PrivateKey, pub, priv)
	}
}

// TestVerifyByID_WebPush_Accepts proves VerifyByID accepts a freshly-minted
// keypair with no network call (the probe is a pure local check).
func TestVerifyByID_WebPush_Accepts(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, _, err := svc.EnsureWebPushKeys(ctx, scope); err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}
	in, err := svc.store.Integrations().GetByProviderID(ctx, scope, "webpush")
	if err != nil {
		t.Fatalf("GetByProviderID: %v", err)
	}

	res, err := svc.VerifyByID(ctx, scope, in.ID)
	if err != nil {
		t.Fatalf("VerifyByID: %v", err)
	}
	if !res.OK {
		t.Errorf("VerifyByID.OK = false, want true (detail=%q)", res.Detail)
	}
	if res.ProviderID != "webpush" {
		t.Errorf("VerifyByID.ProviderID = %q, want %q", res.ProviderID, "webpush")
	}
}

// TestVerifyByID_WebPush_RejectsMismatchedKeypair proves the probe rejects a
// public/private key pair that don't correspond to the same point — the
// derive-and-compare check, not just a decode-succeeds check.
func TestVerifyByID_WebPush_RejectsMismatchedKeypair(t *testing.T) {
	ctx := context.Background()
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	if _, _, err := svc.EnsureWebPushKeys(ctx, scope); err != nil {
		t.Fatalf("EnsureWebPushKeys: %v", err)
	}
	// Mint a second, unrelated keypair and mix its private key with the
	// first scope's public key to break the pairing.
	otherScope := domain.Scope{OrgID: "o2", EnvID: "e2"}
	_, otherPriv, err := svc.EnsureWebPushKeys(ctx, otherScope)
	if err != nil {
		t.Fatalf("EnsureWebPushKeys (other scope): %v", err)
	}

	in, err := svc.store.Integrations().GetByProviderID(ctx, scope, "webpush")
	if err != nil {
		t.Fatalf("GetByProviderID: %v", err)
	}
	pub, _, err := svc.EnsureWebPushKeys(ctx, scope)
	if err != nil {
		t.Fatalf("EnsureWebPushKeys (reread): %v", err)
	}

	mismatched, err := json.Marshal(WebPushConfig{PublicKey: pub, PrivateKey: otherPriv})
	if err != nil {
		t.Fatalf("marshal mismatched config: %v", err)
	}
	enc, err := svc.encrypt(mismatched)
	if err != nil {
		t.Fatalf("encrypt mismatched config: %v", err)
	}
	in.EncCredentials = enc
	if err := svc.store.Integrations().Update(ctx, scope, in); err != nil {
		t.Fatalf("store mismatched config: %v", err)
	}

	res, err := svc.VerifyByID(ctx, scope, in.ID)
	if err != nil {
		t.Fatalf("VerifyByID: %v", err)
	}
	if res.OK {
		t.Errorf("VerifyByID.OK = true for a mismatched keypair, want false (detail=%q)", res.Detail)
	}
}
