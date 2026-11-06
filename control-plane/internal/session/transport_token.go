package session

// POST /v1/sessions/{id}/transport-token: a fresh WebTransport token for a reconnect or a
// takeover. semantics: control-api.md §POST /v1/sessions/{id}/transport-token

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/wtendpoint"
)

var (
	// ErrTransportUnavailable: the session is not WebTransport, its host has no usable endpoint,
	// or its agent refused the token. The client falls back to the signaling token.
	ErrTransportUnavailable = errors.New("webtransport is not available for this session")
	// ErrAgentNoAck: the agent did not confirm the token's hash, so the token is not handed out.
	ErrAgentNoAck = errors.New("the host agent did not acknowledge the transport token")
)

// transportTokenAckTimeout is the contract's 5 s.
const transportTokenAckTimeout = 5 * time.Second

// MintReconnectTransport mints a transport token for sess, delivers its hash to the agent and
// returns the options only after the agent's ack{ok:true}. The endpoint is resolved again so a
// rotated certificate or a changed override is picked up.
func (c *Coordinator) MintReconnectTransport(ctx context.Context, sess Session) (*TransportOptions, error) {
	if sess.Transport != transportWebTransport || sess.HostID == nil {
		return nil, ErrTransportUnavailable
	}
	host, err := wtendpoint.Load(ctx, c.store.pool, *sess.HostID)
	if err != nil {
		return nil, fmt.Errorf("load host endpoint: %w", err)
	}
	endpoint := wtendpoint.Resolve(host, time.Now()).Endpoint
	if endpoint == nil {
		return nil, ErrTransportUnavailable
	}
	grant, err := c.store.MintTransportToken(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	cmd := agentws.SessionTransportTokenCmd{
		Type: "session_transport_token", ID: newCmdID(), SessionID: sess.ID,
		WTTokenHash: grant.Hash, WTTokenExpiresAt: grant.ExpiresAt,
	}
	ackCtx, cancel := context.WithTimeout(ctx, transportTokenAckTimeout)
	defer cancel()
	res, err := c.dispatcher.SendWithAck(ackCtx, *sess.HostID, cmd.ID, cmd)
	switch {
	case err != nil:
		return nil, ErrAgentNoAck
	case !res.OK:
		c.log.Info("transport token refused by the agent", "session_id", sess.ID, "reason", res.Error)
		return nil, ErrTransportUnavailable
	}
	return &TransportOptions{URL: endpoint.URL, Token: grant.Plaintext, ExpiresAt: grant.ExpiresAt, CertHashes: endpoint.CertHashes}, nil
}
