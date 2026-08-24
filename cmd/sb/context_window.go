package main

import (
	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// effectiveContextWindow is the one precedence rule for a concrete target's
// usable context. An enforced live limit wins because the server will reject
// anything larger. A user declaration beats metadata the server did not mark
// as enforced; absent either, the live hint beats the catalog default.
func effectiveContextWindow(cfg *config.Config, reg *providers, cat *catalog.Catalog, target provider.RouteTarget) int {
	catalogWindow := 0
	if cat != nil {
		if info, _, ok := cat.Lookup(target); ok {
			catalogWindow = info.ContextWindow
		}
	}
	return resolveContextWindow(cfg, reg, target, catalogWindow)
}

func resolveContextWindow(cfg *config.Config, reg *providers, target provider.RouteTarget, catalogWindow int) int {
	probed, enforced := 0, false
	if reg != nil {
		probed, enforced = reg.probedContextWindow(target)
	}
	declared := 0
	if cfg != nil {
		declared = cfg.ProviderForTarget(target.Provider, target.Surface).ContextWindow
	}
	switch {
	case probed > 0 && enforced:
		return probed
	case declared > 0:
		return declared
	case probed > 0:
		return probed
	default:
		return catalogWindow
	}
}

// checkRequestContext applies the same final pre-send arithmetic as the agent
// loop to a one-shot request such as compaction, which deliberately bypasses
// the loop. Unknown remains unknown; a finite window with no finite output
// allowance fails closed through EffectiveOutputTokenAllowance.
func checkRequestContext(bound provider.Provider, target provider.RouteTarget, req provider.Request, cat *catalog.Catalog, window int) error {
	if window <= 0 {
		return nil
	}
	catalogMax := 0
	if cat != nil {
		if info, _, ok := cat.Lookup(target); ok {
			catalogMax = info.MaxOutput
		}
	}
	input := prefix.RequestTokenCeiling(req)
	output, err := provider.ResolveOutputTokenAllowance(bound, target, catalogMax)
	if err != nil {
		return err
	}
	if input < 0 || output < 0 || output > window || input > window-output {
		return &agent.ContextWindowError{
			Target: target.ID(), Window: window, InputTokens: input, ReservedOutput: output,
		}
	}
	return nil
}
