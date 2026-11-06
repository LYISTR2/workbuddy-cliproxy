package catalog

type ReasoningPolicy string

const (
	ReasoningCompatible ReasoningPolicy = "compatible"
	ReasoningStrict     ReasoningPolicy = "strict"
)

func (p ReasoningPolicy) Valid() bool { return p == ReasoningCompatible || p == ReasoningStrict }

// Only translate common effort labels, and only to values explicitly listed by
// the model. Disabling reasoning is deliberately outside this ranking.
var standardEfforts = [...]string{"minimal", "low", "medium", "high", "xhigh", "max"}

func effortRank(value string) int {
	for rank, candidate := range standardEfforts {
		if value == candidate {
			return rank
		}
	}
	return -1
}

func supportsEffort(levels []string, effort string) bool {
	for _, level := range levels {
		if level == effort {
			return true
		}
	}
	return false
}

func compatibleEffort(requested string, levels []string) (string, bool) {
	wanted := effortRank(requested)
	if wanted < 0 {
		return "", false
	}
	above, below := "", ""
	aboveRank, belowRank := len(standardEfforts), -1
	for _, candidate := range levels {
		rank := effortRank(candidate)
		if rank < 0 {
			continue
		}
		if rank >= wanted && rank < aboveRank {
			above, aboveRank = candidate, rank
		}
		if rank < wanted && rank > belowRank {
			below, belowRank = candidate, rank
		}
	}
	if above != "" {
		return above, true
	}
	return below, below != ""
}

func (m Model) compatibleEffortMapping() map[string]string {
	levels := m.SupportedEfforts()
	result := map[string]string{}
	for _, requested := range standardEfforts {
		if supportsEffort(levels, requested) {
			continue
		}
		if target, ok := compatibleEffort(requested, levels); ok {
			result[requested] = target
		}
	}
	return result
}
