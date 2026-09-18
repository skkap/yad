package v1

// Result is a run's terminal report. It is written to the runner's outbox
// before the first attempt and retried until acknowledged; a hub applies it at
// most once and answers 409 when it already holds a different terminal state.
type Result struct {
	State     RunState  `json:"state" enum:"succeeded,failed,cancelled,timed_out,lost"`
	FinalText string    `json:"final_text,omitempty"`
	Error     *RunError `json:"error,omitempty"`
	Usage     RunUsage  `json:"usage"`
	Metrics   Metrics   `json:"metrics"`
	LastSeq   int64     `json:"last_seq"`
}

// RunUsage is usage per model for the whole run.
type RunUsage struct {
	ByModel map[string]Usage `json:"by_model,omitempty"`
}

// Metrics is how the run went, for the hub's "why is Codex slow on the Linux
// box" questions.
type Metrics struct {
	DurationMS      int64  `json:"duration_ms"`
	FirstEventMS    int64  `json:"first_event_ms"`
	ToolCalls       int    `json:"tool_calls"`
	APIRetries      int    `json:"api_retries"`
	Stalls          int    `json:"stalls"`
	CancelLatencyMS *int64 `json:"cancel_latency_ms,omitempty"`
	WaitedMS        int64  `json:"waited_ms,omitempty"`
	AccountSwitches int    `json:"account_switches"`
}
