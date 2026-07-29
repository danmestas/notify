package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// runValidateSteps re-validates every stored workflow's steps against the
// CURRENT step-type contracts and reports the ones that no longer conform.
//
// It exists because #258 tightened two authoring contracts — a dynamic delay
// must now declare dynamicKind, and a scheduled digest's cron must be the
// 5-field form the dagnats engine can express — and the decision was to reject
// the loose forms rather than grandfather them. Rejecting them is only safe if
// the set of affected workflows is known BEFORE deploy rather than discovered
// afterwards, which is what this command is for.
//
// It is read-only: it never edits or republishes anything. Exit code 1 when any
// workflow fails, so it can gate a deploy.
func runValidateSteps(args []string) error {
	fs := flag.NewFlagSet("validate-steps", flag.ExitOnError)
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer func() { _ = st.Close() }()

	workflows, err := st.Workflows().ListAll(ctx)
	if err != nil {
		return fmt.Errorf("list workflows: %w", err)
	}

	reg := steptype.Default()
	type failure struct {
		scope    string
		workflow string
		stepID   string
		stepType string
		reason   string
	}
	var failures []failure

	for _, wf := range workflows {
		var steps []domain.Step
		if len(wf.Steps) > 0 {
			if uErr := json.Unmarshal(wf.Steps, &steps); uErr != nil {
				failures = append(failures, failure{
					scope: wf.OrgID + "/" + wf.EnvID, workflow: wf.TriggerIdentifier,
					reason: fmt.Sprintf("steps JSON is unreadable: %v", uErr),
				})
				continue
			}
		}
		for _, step := range steps {
			sType := reg.Get(step.Type)
			if sType == nil {
				failures = append(failures, failure{
					scope: wf.OrgID + "/" + wf.EnvID, workflow: wf.TriggerIdentifier,
					stepID: step.StepID, stepType: step.Type,
					reason: "unknown step type",
				})
				continue
			}
			if vErr := sType.Validate(step.ControlValues); vErr != nil {
				failures = append(failures, failure{
					scope: wf.OrgID + "/" + wf.EnvID, workflow: wf.TriggerIdentifier,
					stepID: step.StepID, stepType: step.Type,
					reason: vErr.Error(),
				})
			}
		}
	}

	sort.Slice(failures, func(i, j int) bool {
		if failures[i].scope != failures[j].scope {
			return failures[i].scope < failures[j].scope
		}
		if failures[i].workflow != failures[j].workflow {
			return failures[i].workflow < failures[j].workflow
		}
		return failures[i].stepID < failures[j].stepID
	})

	fmt.Printf("checked %d workflow(s)\n", len(workflows))
	if len(failures) == 0 {
		fmt.Println("all stored workflows conform to the current step contracts")
		return nil
	}

	// Print every failure with the scope, workflow, step and reason — enough to
	// go and fix it. A bare count would tell an operator they have a problem
	// without telling them where.
	fmt.Fprintf(os.Stderr, "\n%d step(s) no longer conform:\n\n", len(failures))
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "  %s  workflow=%s  step=%s (%s)\n      %s\n",
			f.scope, f.workflow, f.stepID, f.stepType, f.reason)
	}
	fmt.Fprintf(os.Stderr, "\nThese workflows cannot be re-published until fixed.\n")
	return fmt.Errorf("%d non-conforming step(s)", len(failures))
}
