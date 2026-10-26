package domain

// These observations belong to a connection, not a subscription batch.
type ConnectivityState struct {
	ProbeTarget      string `json:"probeTarget,omitempty"`
	KernelGeneration string `json:"kernelGeneration,omitempty"`
	FailedRounds     int    `json:"failedRounds,omitempty"`
	FirstFailureAt   string `json:"firstFailureAt,omitempty"`
	LastFailureAt    string `json:"lastFailureAt,omitempty"`
	CheckedAt        string `json:"checkedAt"`
	Reachable        bool   `json:"reachable"`
	ConfirmedDead    bool   `json:"confirmedDead,omitempty"`
	Error            string `json:"error,omitempty"`
}

type ExitState struct {
	Incarnation      string `json:"incarnation,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	FailureStreak    int    `json:"failureStreak,omitempty"`
	ProbeMillis      int64  `json:"probeMillis,omitempty"`
	KernelGeneration string `json:"kernelGeneration,omitempty"`
	IP               string `json:"ip,omitempty"`
	CheckedAt        string `json:"checkedAt"`
	Version          int64  `json:"version"`
	Source           string `json:"source,omitempty"`
	Country          string `json:"country,omitempty"`
	AddressFamily    int    `json:"addressFamily,omitempty"`
	Verified         bool   `json:"verified"`
	Error            string `json:"error,omitempty"`
}

type LifecycleState struct {
	Status    string `json:"status"`
	RetiredAt string `json:"retiredAt,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
