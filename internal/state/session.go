package state

import "time"

type Activity struct {
	Kind    string    `json:"kind"`
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
}

type Pending struct {
	ToolUseID    string    `json:"tool_use_id"`
	Tool         string    `json:"tool"`
	InputPreview string    `json:"input_preview"`
	DetectedAt   time.Time `json:"detected_at"`
}

type Session struct {
	WrapperPID   int       `json:"wrapper_pid"`
	ClaudePID    int       `json:"claude_pid"`
	SessionID    string    `json:"session_id,omitempty"`
	CWD          string    `json:"cwd,omitempty"`
	Status       string    `json:"status,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	SocketPath   string    `json:"socket_path"`
	AutoYes      bool      `json:"auto_yes"`
	LastActivity *Activity `json:"last_activity,omitempty"`
	Pending      *Pending  `json:"pending,omitempty"`
}
