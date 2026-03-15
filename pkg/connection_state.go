package graft

import graftv1 "github.com/edaniels/graft/gen/proto/graft/v1"

// ConnectionState is the tiny state machine a connection goes through.
type ConnectionState byte

const (
	// ConnectionStateNew is the initial zero-value state of a freshly created daemon.
	ConnectionStateNew ConnectionState = iota
	ConnectionStateInitializing
	ConnectionStateConnected
	ConnectionStateFailed
	ConnectionStateClosed
	ConnectionStateReconnecting
)

func (s ConnectionState) String() string {
	switch s {
	case ConnectionStateNew:
		return "New"
	case ConnectionStateInitializing:
		return "Initializing"
	case ConnectionStateConnected:
		return "Connected"
	case ConnectionStateFailed:
		return "Failed"
	case ConnectionStateClosed:
		return "Closed"
	case ConnectionStateReconnecting:
		return "Reconnecting"
	default:
		return "<Unknown>"
	}
}

func (s ConnectionState) Proto() graftv1.ConnectionState {
	switch s {
	case ConnectionStateNew:
		// No proto-level NEW state; report as Initializing since New is a
		// transient pre-initialization state that external clients never observe.
		return graftv1.ConnectionState_CONNECTION_STATE_INITIALIZING
	case ConnectionStateInitializing:
		return graftv1.ConnectionState_CONNECTION_STATE_INITIALIZING
	case ConnectionStateConnected:
		return graftv1.ConnectionState_CONNECTION_STATE_CONNECTED
	case ConnectionStateFailed:
		return graftv1.ConnectionState_CONNECTION_STATE_FAILED
	case ConnectionStateClosed:
		return graftv1.ConnectionState_CONNECTION_STATE_CLOSED
	case ConnectionStateReconnecting:
		return graftv1.ConnectionState_CONNECTION_STATE_RECONNECTING
	default:
		return graftv1.ConnectionState_CONNECTION_STATE_UNKNOWN
	}
}
