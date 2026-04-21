package gating

// Decision is the outcome of the gating pipeline.
type Decision int

const (
	DecisionIgnore Decision = iota
	DecisionAllow
	DecisionDelete
)

func (d Decision) String() string {
	switch d {
	case DecisionIgnore:
		return "ignore"
	case DecisionAllow:
		return "allow"
	case DecisionDelete:
		return "delete"
	}
	return "unknown"
}

// Reason is a short, machine-readable cause for the decision (used in structured logs).
type Reason string

const (
	ReasonNoBinding         Reason = "no_binding"
	ReasonServiceMessage    Reason = "service_message"
	ReasonChannelRootPost   Reason = "channel_root_post"
	ReasonOtherSenderChat   Reason = "other_sender_chat"
	ReasonBotAllowlist      Reason = "bot_allowlist"
	ReasonAnonymousAdmin    Reason = "anonymous_admin"
	ReasonCommand           Reason = "command"
	ReasonCacheHit          Reason = "cache_hit"
	ReasonMember            Reason = "member"
	ReasonNotMember         Reason = "not_member"
	ReasonMediaGroupDedup   Reason = "media_group_dedup"
	ReasonErrorDefaultAllow Reason = "error_default_allow"
	ReasonMissingSender     Reason = "missing_sender"
)
