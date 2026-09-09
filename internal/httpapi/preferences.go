package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// preferencesPutLocks serializes putSubscriberPreferences's read-merge-write
// per (scope, subscriber): the preferences store has no merge-at-write
// primitive (Upsert replaces the whole channels blob — see
// internal/store/preference_repo.go), so two concurrent partial PUTs for the
// same subscriber can each read the pre-update row and the later write drops
// the earlier writer's key. This is a process-local keyed mutex — correct for
// notify's single-binary deployment; a multi-instance deployment would need a
// DB-level lock or a store-side merge instead.
var preferencesPutLocks sync.Map // key: scope.OrgID+"/"+scope.EnvID+"/"+subscriberID -> *sync.Mutex

// preferencesPutLockFor returns the mutex guarding PUT preferences for this
// scope+subscriber, creating one on first use.
func preferencesPutLockFor(scope domain.Scope, subscriberID string) *sync.Mutex {
	key := scope.OrgID + "/" + scope.EnvID + "/" + subscriberID
	v, _ := preferencesPutLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// preferenceChannelNames enumerates the channel keys accepted and reported by
// the subscriber preferences route. It mirrors the five delivery channels
// (internal/steptype's in_app, email, push, sms, chat) — the same set
// preferences.Service.ChannelEnabled is consulted for at delivery time.
var preferenceChannelNames = []string{"in_app", "push", "email", "sms", "chat"}

// isKnownPreferenceChannel reports whether name is one of preferenceChannelNames.
func isKnownPreferenceChannel(name string) bool {
	for _, c := range preferenceChannelNames {
		if c == name {
			return true
		}
	}
	return false
}

// subscriberPreferencesDTO is the GET/PUT /v1/subscribers/{id}/preferences
// JSON shape. Global always reports every known channel explicitly (missing
// => enabled, matching preferences.Service's own default). Workflows is keyed
// by TRIGGER IDENTIFIER (not the internal workflow row id) and lists only the
// channels explicitly overridden for that workflow — an omitted channel means
// "inherit global".
type subscriberPreferencesDTO struct {
	Global    map[string]bool            `json:"global"`
	Workflows map[string]map[string]bool `json:"workflows"`
	// Digests is a SIBLING of Workflows rather than a key inside it, keyed the
	// same way by trigger identifier (#336). Workflows answers "which channels
	// are on"; a digest window answers "how often are they batched" -- two
	// different questions, and nesting the second inside the first would turn
	// that map[string]bool into a union, breaking every client decoding it on a
	// GET consumers make on every settings-page load.
	//
	// On GET this reports the EFFECTIVE window for every workflow that has a
	// regular digest step, with Source naming where it came from -- mirroring
	// how Global reports every known channel resolved rather than only stored
	// rows, so a consumer can display the cadence in force without re-deriving
	// it. On PUT only amount and unit are read; a null value clears the
	// override.
	Digests map[string]*digestWindowDTO `json:"digests,omitempty"`
}

// digestWindowDTO is one digest cadence. Source is GET-only ("subscriber" when
// the subscriber has overridden, "workflow" when the step's authored window
// applies) and is ignored on PUT.
type digestWindowDTO struct {
	Amount int64  `json:"amount"`
	Unit   string `json:"unit"`
	Source string `json:"source,omitempty"`
}

// firstUnknownChannel reports the first key in m that is not a known
// preference channel, or "" if every key is known.
func firstUnknownChannel(m map[string]bool) string {
	for ch := range m {
		if !isKnownPreferenceChannel(ch) {
			return ch
		}
	}
	return ""
}

// getSubscriberPreferences handles GET /v1/subscribers/{id}/preferences.
// {id} is the subscriber's external subscriberId — the same identifier
// resolved by PUT /v1/subscribers/{subscriberId}/credentials and DELETE
// .../credentials/webpush — resolved within the request's org/env Scope; an
// id belonging to another org/env answers 404, same as an id that does not
// exist at all.
func getSubscriberPreferences(aud *audience.Service, au *authoring.Service, prefs *preferences.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())

		sub, err := aud.Get(r.Context(), scope, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "subscriber not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}

		rows, err := prefs.GetForSubscriber(r.Context(), scope, sub.SubscriberID)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}

		dto, err := buildPreferencesDTO(r.Context(), au, scope, rows)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	}
}

// buildPreferencesDTO renders the stored preference rows into the response
// shape: Global defaults every known channel to enabled and overlays the
// stored subscriberGlobal row; Workflows translates each stored workflow-id
// row to its trigger identifier and lists only the stored explicit overrides.
// A workflow row whose workflow has since been deleted is silently omitted —
// there is no trigger identifier left to key it by.
func buildPreferencesDTO(ctx context.Context, au *authoring.Service, scope domain.Scope, rows []domain.Preference) (subscriberPreferencesDTO, error) {
	global := make(map[string]bool, len(preferenceChannelNames))
	for _, ch := range preferenceChannelNames {
		global[ch] = true
	}
	workflows := make(map[string]map[string]bool)

	for _, row := range rows {
		// Only channel-map rows decode here. A "workflowDigest" row stores an
		// {amount,unit} object in the same blob column and would fail this
		// unmarshal, taking the whole preferences read down with it.
		if row.Type != "subscriberGlobal" && row.Type != "workflow" {
			continue
		}
		var channels map[string]bool
		if len(row.Channels) > 0 {
			if err := json.Unmarshal(row.Channels, &channels); err != nil {
				return subscriberPreferencesDTO{}, err
			}
		}
		if row.Type == "subscriberGlobal" {
			for ch, v := range channels {
				global[ch] = v
			}
			continue
		}
		if len(channels) == 0 {
			continue
		}
		// Per-workflow row: translate the stored workflow id to its trigger
		// identifier so the response is keyed the way clients trigger.
		wf, err := au.GetByID(ctx, scope, row.WorkflowID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return subscriberPreferencesDTO{}, err
		}
		workflows[wf.TriggerIdentifier] = channels
	}

	digests, err := buildDigestsDTO(ctx, au, scope, rows)
	if err != nil {
		return subscriberPreferencesDTO{}, err
	}
	return subscriberPreferencesDTO{Global: global, Workflows: workflows, Digests: digests}, nil
}

// buildDigestsDTO reports the effective digest window for every workflow in the
// environment that has a regular digest step: the subscriber's override when
// they have one, else the step's authored window. A workflow with no digest
// step is absent, so the map answers "which cadences can this subscriber set"
// as well as "which is in force".
func buildDigestsDTO(ctx context.Context, au *authoring.Service, scope domain.Scope, rows []domain.Preference) (map[string]*digestWindowDTO, error) {
	overrides := make(map[string]domain.DigestWindow)
	for _, row := range rows {
		if row.Type != "workflowDigest" || len(row.Channels) == 0 {
			continue
		}
		var w domain.DigestWindow
		if err := json.Unmarshal(row.Channels, &w); err != nil {
			return nil, err
		}
		if !w.IsZero() {
			overrides[row.WorkflowID] = w
		}
	}

	wfs, err := au.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*digestWindowDTO)
	for _, wf := range wfs {
		authored, ok, wErr := firstDigestWindow(wf)
		if wErr != nil || !ok {
			// A workflow whose digest step will not parse is reported as having
			// no window rather than failing the read: one bad step must not
			// make a subscriber unable to see any of their settings.
			continue
		}
		window, source := authored, "workflow"
		if o, has := overrides[wf.ID]; has {
			window, source = o, "subscriber"
		}
		out[wf.TriggerIdentifier] = &digestWindowDTO{Amount: window.Amount, Unit: window.Unit, Source: source}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// firstDigestWindow reports the authored window of a workflow's first digest
// step. ok is false when the workflow has no digest step, or only a scheduled
// (cron) one -- a schedule is not a duration and an {amount,unit} override
// cannot express it.
func firstDigestWindow(wf domain.Workflow) (domain.DigestWindow, bool, error) {
	if len(wf.Steps) == 0 {
		return domain.DigestWindow{}, false, nil
	}
	var steps []domain.Step
	if err := json.Unmarshal(wf.Steps, &steps); err != nil {
		return domain.DigestWindow{}, false, err
	}
	for _, st := range steps {
		if st.Type != "digest" {
			continue
		}
		w, scheduled, err := steptype.DigestWindow(st.ControlValues)
		if err != nil || scheduled {
			return domain.DigestWindow{}, false, err
		}
		return w, true, nil
	}
	return domain.DigestWindow{}, false, nil
}

// putSubscriberPreferences handles PUT /v1/subscribers/{id}/preferences.
// The request body is the same shape as the GET response; both "global" and
// each entry in "workflows" are partial updates — a channel key omitted from
// the request leaves the previously stored value (if any) untouched at that
// level. An unknown trigger identifier or unknown channel name fails the
// whole request closed with 422 before any write happens.
func putSubscriberPreferences(aud *audience.Service, au *authoring.Service, prefs *preferences.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		scope := identity.ScopeFrom(r.Context())

		var req subscriberPreferencesDTO
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}

		sub, err := aud.Get(r.Context(), scope, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "subscriber not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}

		// Validate everything before writing anything: unknown channel names
		// (global or per-workflow) and unknown trigger identifiers both fail
		// the whole PUT closed with 422.
		if ch := firstUnknownChannel(req.Global); ch != "" {
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "unknown channel "+ch)
			return
		}
		workflowIDs := make(map[string]string, len(req.Workflows)) // triggerIdentifier -> workflow id
		var unknown []string
		for triggerID, channels := range req.Workflows {
			if ch := firstUnknownChannel(channels); ch != "" {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "unknown channel "+ch+" for workflow "+triggerID)
				return
			}
			wf, err := au.GetBySlug(r.Context(), scope, triggerID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					unknown = append(unknown, triggerID)
					continue
				}
				writeInternalError(w, r, err)
				return
			}
			workflowIDs[triggerID] = wf.ID
		}
		if len(unknown) > 0 {
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity",
				"unknown trigger identifier(s): "+strings.Join(unknown, ", "))
			return
		}
		// Digest overrides validate the same way and in the same pass: unknown
		// identifier, a workflow with no regular digest step, or an unusable
		// amount/unit all fail the whole PUT closed before anything is written.
		// Refusing a window on a workflow that never digests is worth the extra
		// lookup -- it is a setting that would otherwise be stored, read back,
		// and never do anything.
		digestWorkflowIDs := make(map[string]string, len(req.Digests))
		for triggerID, window := range req.Digests {
			wf, wErr := au.GetBySlug(r.Context(), scope, triggerID)
			if wErr != nil {
				if errors.Is(wErr, store.ErrNotFound) {
					writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity",
						"unknown trigger identifier: "+triggerID)
					return
				}
				writeInternalError(w, r, wErr)
				return
			}
			if _, ok, fErr := firstDigestWindow(wf); fErr != nil || !ok {
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity",
					"workflow "+triggerID+" has no regular digest step to set a window on")
				return
			}
			if window != nil {
				if window.Amount <= 0 {
					writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity",
						"digest amount for "+triggerID+" must be positive")
					return
				}
				if _, uErr := steptype.UnitDuration(window.Unit); uErr != nil {
					writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity",
						"digest unit for "+triggerID+": "+uErr.Error())
					return
				}
			}
			digestWorkflowIDs[triggerID] = wf.ID
		}

		// Serialize the read-merge-write below per (scope, subscriber): without
		// this, two concurrent partial PUTs can each read the same pre-update
		// rows and the later write silently drops the earlier writer's key.
		lock := preferencesPutLockFor(scope, sub.SubscriberID)
		lock.Lock()
		defer lock.Unlock()

		// Load existing rows to merge partial updates onto (do not clobber
		// unspecified keys).
		existingRows, err := prefs.GetForSubscriber(r.Context(), scope, sub.SubscriberID)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		existingGlobal := map[string]bool{}
		existingByWorkflowID := map[string]map[string]bool{}
		for _, row := range existingRows {
			// Same filter as buildPreferencesDTO: a "workflowDigest" row holds
			// an {amount,unit} object in this blob column and must not be
			// decoded as a channel map. Missing it here 500s any PUT made by a
			// subscriber who has a digest override.
			if row.Type != "subscriberGlobal" && row.Type != "workflow" {
				continue
			}
			var channels map[string]bool
			if len(row.Channels) > 0 {
				if err := json.Unmarshal(row.Channels, &channels); err != nil {
					writeInternalError(w, r, err)
					return
				}
			}
			if row.Type == "subscriberGlobal" {
				existingGlobal = channels
			} else {
				existingByWorkflowID[row.WorkflowID] = channels
			}
		}

		if len(req.Global) > 0 {
			merged := mergeChannels(existingGlobal, req.Global)
			if err := prefs.SetGlobal(r.Context(), scope, sub.SubscriberID, merged); err != nil {
				writeInternalError(w, r, err)
				return
			}
		}
		for triggerID, channels := range req.Workflows {
			workflowID := workflowIDs[triggerID]
			merged := mergeChannels(existingByWorkflowID[workflowID], channels)
			if err := prefs.SetForWorkflow(r.Context(), scope, sub.SubscriberID, workflowID, merged); err != nil {
				writeInternalError(w, r, err)
				return
			}
		}

		for triggerID, window := range req.Digests {
			workflowID := digestWorkflowIDs[triggerID]
			// An explicit null clears the override so the step's own window
			// applies again. Omitting the key leaves it untouched, matching how
			// every other field in this PUT merges rather than replaces.
			if window == nil {
				if err := prefs.ClearDigestWindow(r.Context(), scope, sub.SubscriberID, workflowID); err != nil {
					writeInternalError(w, r, err)
					return
				}
				continue
			}
			if err := prefs.SetDigestWindow(r.Context(), scope, sub.SubscriberID, workflowID,
				domain.DigestWindow{Amount: window.Amount, Unit: window.Unit}); err != nil {
				writeInternalError(w, r, err)
				return
			}
		}

		rows, err := prefs.GetForSubscriber(r.Context(), scope, sub.SubscriberID)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		dto, err := buildPreferencesDTO(r.Context(), au, scope, rows)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, dto)
	}
}

// mergeChannels returns a copy of existing with each key in updates applied,
// overwriting on conflict. existing may be nil.
func mergeChannels(existing, updates map[string]bool) map[string]bool {
	merged := make(map[string]bool, len(existing)+len(updates))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range updates {
		merged[k] = v
	}
	return merged
}
