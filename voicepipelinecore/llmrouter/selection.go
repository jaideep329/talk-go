package llmrouter

import (
	"context"
	"sort"
)

// selection is the resolved endpoint choice for one turn.
type selection struct {
	ConfigKey     string
	UsingFallback bool
	SelectedGroup string
}

// getFastestForGroup mirrors LLMSwitchingService.get_fastest_for_group:
//
//   - score every region-matching config in the group by selection
//     latency (mean of last 5 adjusted poll latencies), skipping
//     blacklisted endpoints and those with no latency data;
//   - pick the fastest;
//   - if none is available and the group is not already the fallback
//     group, repeat against the gpt-4.1 fallback group (using_fallback);
//   - as a last resort return the fallback group's hardcoded fallback
//     config so a turn can always proceed.
func getFastestForGroup(ctx context.Context, store RedisStore, group, region string) (selection, bool) {
	configs, ok := groupConfigsForRegion(group, region)
	if !ok || len(configs) == 0 {
		return selection{}, false
	}

	if key, ok := fastestAvailable(ctx, store, configs); ok {
		return selection{ConfigKey: key, UsingFallback: false, SelectedGroup: group}, true
	}

	if group != fallbackModelGroup {
		fbConfigs, fbOK := groupConfigsForRegion(fallbackModelGroup, region)
		if fbOK && len(fbConfigs) > 0 {
			if key, ok := fastestAvailable(ctx, store, fbConfigs); ok {
				return selection{ConfigKey: key, UsingFallback: true, SelectedGroup: fallbackModelGroup}, true
			}
			if fb := modelGroups[fallbackModelGroup].Fallback; fb != "" {
				return selection{ConfigKey: fb, UsingFallback: true, SelectedGroup: fallbackModelGroup}, true
			}
		}
	}

	if fb := modelGroups[group].Fallback; fb != "" {
		return selection{ConfigKey: fb, UsingFallback: false, SelectedGroup: group}, true
	}
	return selection{}, false
}

// fastestAvailable reads the health of every config via one MGET, drops
// blacklisted endpoints and those with no latency data, and returns the
// key with the lowest selection latency.
func fastestAvailable(ctx context.Context, store RedisStore, configs []endpointConfig) (string, bool) {
	keys := make([]string, len(configs))
	for i, cfg := range configs {
		keys[i] = healthKey(cfg.Key)
	}
	raws, err := store.MGetCache(ctx, keys...)
	if err != nil || len(raws) != len(configs) {
		return "", false
	}

	type candidate struct {
		key     string
		latency float64
	}
	candidates := make([]candidate, 0, len(configs))
	for i, cfg := range configs {
		health := parseHealth(raws[i])
		if health.Blacklisted {
			continue
		}
		latency, ok := health.selectionLatency()
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{key: cfg.Key, latency: latency})
	}
	if len(candidates) == 0 {
		return "", false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].latency < candidates[j].latency
	})
	return candidates[0].key, true
}
