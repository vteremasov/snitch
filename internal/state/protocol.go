package state

type Op string

const (
	OpGetState   Op = "get_state"
	OpSetAutoYes Op = "set_auto_yes"
	OpApproveNow Op = "approve_now"
	OpSubscribe  Op = "subscribe"
)

type Request struct {
	Op Op    `json:"op"`
	On *bool `json:"on,omitempty"`
}

type Response struct {
	Ok      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	Event   string   `json:"event,omitempty"`
	Session *Session `json:"session,omitempty"`
}
