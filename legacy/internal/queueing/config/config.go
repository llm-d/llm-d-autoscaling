package config

// options for allocation under saturated condition
type SaturatedAllocationPolicy int

const (
	None               SaturatedAllocationPolicy = iota // 0 : no additional allocation beyond satisfying SLOs
	PriorityExhaustive                                  // 1 : allocating exhaustively to servers in priority ordering
	PriorityRoundRobin                                  // 2 : allocating in round-robin fashion within priority groups
	RoundRobin                                          // 3 : allocating in round-robin fashion across all servers
)

func (p SaturatedAllocationPolicy) String() string {
	switch p {
	case None:
		return "None"
	case PriorityExhaustive:
		return "PriorityExhaustive"
	case PriorityRoundRobin:
		return "PriorityRoundRobin"
	case RoundRobin:
		return "RoundRobin"
	default:
		return "Unknown"
	}
}

func SaturatedAllocationPolicyEnum(s string) SaturatedAllocationPolicy {
	switch s {
	case "None":
		return None
	case "PriorityExhaustive":
		return PriorityExhaustive
	case "PriorityRoundRobin":
		return PriorityRoundRobin
	case "RoundRobin":
		return RoundRobin
	default:
		return DefaultSaturatedAllocationPolicy
	}
}
