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

	return subscriberPreferencesDTO{Global: global, Workflows: workflows}, nil
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
