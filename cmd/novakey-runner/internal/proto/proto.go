package proto

// Wire protocol between novakeyd and novakey-runner.
// Keep this intentionally small and boring.

type ExecRequest struct {
	V int `json:"v"`
	// Correlation ID from the daemon (uuid or similar)
	Req string `json:"req"`
	Action string `json:"action"`
	Params map[string]any `json:"params,omitempty"`
	// Optional metadata (for audit) - runner should NOT trust these for auth.
	InvokedBy *InvokedBy `json:"invoked_by,omitempty"`
}

type InvokedBy struct {
	DeviceID string `json:"device_id,omitempty"`
	Remote   string `json:"remote,omitempty"`
}

type ExecResponse struct {
	V int `json:"v"`
	Req string `json:"req"`
	OK bool `json:"ok"`
	Error string `json:"error,omitempty"`
	ExitCode int `json:"exit_code,omitempty"`
	DurationMS int64 `json:"duration_ms,omitempty"`
	StdoutB64 string `json:"stdout_b64,omitempty"`
	StderrB64 string `json:"stderr_b64,omitempty"`
}
