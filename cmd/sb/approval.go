package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/approval"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// lazyCommandReviewer avoids probing or authenticating another model until
// auto mode actually has a command to review. The first successful binding is
// stable for the process so reviewer identity in the audit does not drift.
type lazyCommandReviewer struct {
	mu        sync.Mutex
	loop      *agent.Loop
	config    *config.Config
	catalog   *catalog.Catalog
	providers *providers
	fallback  config.Tier
	budget    *budgetState
	reviewer  *approval.ModelReviewer
}

func wireCommandReviewer(loop *agent.Loop, cfg *config.Config, cat *catalog.Catalog, reg *providers, fallback config.Tier, budget *budgetState) {
	loop.Perms.SetReviewer(&lazyCommandReviewer{
		loop: loop, config: cfg, catalog: cat, providers: reg, fallback: fallback, budget: budget,
	})
}

func (r *lazyCommandReviewer) Review(ctx context.Context, req permission.ReviewRequest) (permission.ReviewResult, error) {
	reviewer, err := r.get(ctx)
	if err != nil {
		return permission.ReviewResult{}, err
	}
	return reviewer.Review(ctx, req)
}

func (r *lazyCommandReviewer) get(ctx context.Context) (*approval.ModelReviewer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reviewer != nil {
		return r.reviewer, nil
	}
	if r.loop == nil || r.config == nil || r.catalog == nil || r.providers == nil || r.budget == nil {
		return nil, errors.New("command reviewer is missing runtime accounting or provider state")
	}

	candidates, explicit, err := approverCandidates(r.config, r.fallback)
	if err != nil {
		return nil, err
	}
	var failures []string
	for _, candidate := range candidates {
		if err := destinationAllowed(r.config, candidate.Target); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		probed, client, probeErr := r.providers.probeTier(ctx, candidate)
		if probeErr != nil {
			failures = append(failures, candidate.ID+": "+probeErr.Error())
			if explicit {
				break
			}
			continue
		}
		identity := candidate.ID + " " + probed.Target.Display()
		r.reviewer = &approval.ModelReviewer{
			Provider: client,
			Target:   probed.Target,
			Identity: identity,
			Meter: func(target provider.RouteTarget, request provider.Request) (approval.AttemptFinish, error) {
				finish, meterErr := beginMeteredCall(r.budget, r.catalog, r.loop.Session, target, request, session.UsagePurposeApproval, client)
				if meterErr != nil {
					return nil, meterErr
				}
				return approval.AttemptFinish(finish), nil
			},
		}
		return r.reviewer, nil
	}
	return nil, fmt.Errorf("no command approver target is reachable: %s", strings.Join(failures, "; "))
}

// approverCandidates resolves [slots] approver when present. Without it the
// ladder is tried bottom-up, making the cheapest feasible rung the default.
func approverCandidates(cfg *config.Config, fallback config.Tier) ([]config.Tier, bool, error) {
	if ref, ok := cfg.Slots["approver"]; ok {
		if tier, found := cfg.Tier(ref); found {
			return []config.Tier{tier}, true, nil
		}
		target, err := config.ParseTarget(ref, "", "")
		if err != nil {
			return nil, true, fmt.Errorf("[slots] approver %q: %w", ref, err)
		}
		return []config.Tier{{ID: "-approver", Label: "approver", Target: target}}, true, nil
	}
	if len(cfg.Tiers) > 0 {
		return append([]config.Tier(nil), cfg.Tiers...), false, nil
	}
	if fallback.Target.Provider != "" {
		return []config.Tier{fallback}, false, nil
	}
	return nil, false, errors.New("auto mode needs a configured tier or [slots] approver target")
}
