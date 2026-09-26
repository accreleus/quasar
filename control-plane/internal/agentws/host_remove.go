package agentws

import "context"

// HostRemoveCmd is agent-api.md `host_remove` (amendment 14): the host's recovery
// actor removes its node agent, then itself. Nothing follows the ack.
type HostRemoveCmd struct {
	Type      string `json:"type"` // "host_remove"
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
}

// SendHostRemove dispatches a host_remove and waits for the ack. The command id is
// the request id, as for release_apply. An error means undeliverable, or no ack
// before ctx expired: the agent predates the amendment.
func (r *Registry) SendHostRemove(ctx context.Context, hostID, requestID string) (AckResult, error) {
	return r.SendWithAck(ctx, hostID, requestID, HostRemoveCmd{
		Type:      "host_remove",
		ID:        requestID,
		RequestID: requestID,
	})
}
