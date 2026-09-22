package v1

// Result is a run's terminal report. It is written to the runner's outbox
// before the first attempt and retried until acknowledged; a hub applies it at
// most once and answers 409 when it already holds a different terminal state.
type Result struct {
	State     RunState  `json:"state" enum:"succeeded,failed,cancelled,timed_out,lost" doc:"How the run ended. lost here is the runner's own report of a run a previous process held and could not finish."`
	FinalText string    `json:"final_text,omitempty" doc:"The harness's last answer, at most 1 MiB."`
	Error     *RunError `json:"error,omitempty" doc:"Why the run did not succeed. A failed result with class refused is the runner declining a run it was offered, before claiming it."`
	Usage     RunUsage  `json:"usage"`
	Metrics   Metrics   `json:"metrics"`
	LastSeq   int64     `json:"last_seq" doc:"The seq of the run's last event, 0 when it had none. The run's event stream is complete once acked_through reaches it."`
}

// RunUsage is usage per model for the whole run.
type RunUsage struct {
	ByModel map[string]Usage `json:"by_model,omitempty" doc:"Tokens for the whole run, every turn and every account, by model."`
}

// Metrics is how the run went, for the hub's "why is Codex slow on the Linux
// box" questions.
//
// Every figure covers the whole run, however many turns and however many
// accounts it took — not the last turn. The exception is first_event_ms,
// which is one turn's by design; its own comment says why.
//
// A run reported **lost** is the one case where less is known, and the
// difference is worth stating rather than leaving a hub to infer it. Nobody
// watched that run stop, so its figures are what the runner had durably
// recorded, not what happened: duration_ms and waited_ms come from its
// stored timestamps and are floors, while the counters and the usage cover
// every turn that ended before the runner stopped, and account_switches every
// move it made. The turn the runner stopped during is missing from them:
// a harness reports usage when its turn ends, so nothing about that turn was
// ever there to write down. So a zero is what was recorded, not proof that
// nothing happened: no turn may have ended, or the turns that ended reported
// none.
type Metrics struct {
	// DurationMS is how long the run took, from the moment it first reached
	// preparing.
	//
	// For a run reported lost it is a **floor**, not an end anybody saw: the
	// runner's process was gone and nothing observed the run stop, so the
	// duration is measured to the last moment the runner is known to have
	// been alive for it — its last spooled event, or the last write to its
	// row. The real duration is that or more. Measuring to the restart
	// instead would report how long the machine was off.
	DurationMS int64 `json:"duration_ms" doc:"How long the run took, from the moment it first reached preparing. For a run reported lost this is a floor rather than an end anybody observed: nothing watched the run stop, so it is measured to the last moment the runner is known to have been alive for it, and the real duration is that or more. A lost run's other figures are likewise only what the runner had durably recorded: its counters and usage cover every turn that ended before the runner stopped, and account_switches every move it made, but not the turn it stopped during, whose usage the harness never reported. A zero there is what was recorded rather than proof that nothing happened."`
	// FirstEventMS is how long the harness took to say anything, measured
	// from the start of the turn that answered — deliberately not from the
	// start of the run. A run parked five hours on a usage limit and then
	// answering in two seconds is not a harness that took five hours to
	// speak, and the wait is already reported as waited_ms.
	FirstEventMS int64 `json:"first_event_ms" doc:"How long the harness took to say anything, measured from the start of the turn that answered rather than from the start of the run. A run parked on a usage limit and then answering at once is not a slow harness, and the wait is reported separately as waited_ms."`
	ToolCalls    int   `json:"tool_calls" doc:"Tool calls across every turn of the run."`
	// APIRetries counts transient throttling the harness retried by itself.
	// A usage limit is not one of these (DOMAIN.md, "Usage limit").
	APIRetries      int    `json:"api_retries" doc:"Transient throttling the harness retried by itself, across every turn. A usage limit is not one of these."`
	Stalls          int    `json:"stalls" doc:"Times a watchdog found the event stream idle, across every turn."`
	CancelLatencyMS *int64 `json:"cancel_latency_ms,omitempty" doc:"From a cancel or interrupt arriving to the turn ending. Absent when nothing asked the run to stop."`
	// WaitedMS is how long the run spent waiting for an account, **summed
	// over every wait** — not the longest and not the last. A run may be
	// parked, resumed, limited again and parked again, and this is the
	// total of all of it.
	//
	// The total is also what Run.MaxWaitMS is judged against. A cap on the
	// total beside a metric reporting the last wait would let a run time out
	// at a number its own report contradicts, which is not a discrepancy
	// anybody would find before it happened.
	//
	// For a run reported lost while it was parked, this is a floor for the
	// same reason DurationMS is.
	WaitedMS int64 `json:"waited_ms,omitempty" doc:"How long the run spent waiting for a free account, summed over every wait rather than the longest or the last: a run may be parked, resumed, limited again and parked again. This total is also what the run's max_wait_ms is judged against. For a run reported lost while it was parked it is a floor, as duration_ms is."`
	// AccountSwitches is how many turns of this run started on a different
	// account than the turn before them.
	//
	// Defined by cost rather than by cause: a run resumed onto a different
	// account after waiting is counted the same as one a failover moved,
	// because what the two have in common is what the move costs. Two
	// consequences worth stating: a run parked five times and resumed on
	// the same account each time reports 0, and a run that goes A to B and
	// back to A reports 2.
	//
	// The cost is expected to be one cache-cold turn. That is an inference
	// and not a measurement: prompt caches are documented as isolated per
	// account, no cross-account resume has been measured, and decision 0013
	// tracks the gap as DEV-58. The count is worth having either way — it
	// is how many times the run changed accounts — but a hub pricing it
	// should know the price is not yet observed.
	AccountSwitches int `json:"account_switches" doc:"How many turns of this run started on a different account than the turn before them. Counted by what a move costs rather than by what caused it, so a run resumed onto a different account after waiting counts the same as one a failover moved. A run parked five times and resumed on the same account each time reports 0; a run that goes A to B and back to A reports 2. The cost is expected to be one cache-cold turn, because prompt caches are documented as isolated per account -- but no cross-account resume has been measured, so that price is inferred rather than observed."`
}
