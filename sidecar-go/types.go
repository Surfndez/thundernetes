package main

// GameState represents the current state of the game.
type GameState string

// GameOperation represents the type of operation that the GSDK shoud do next
type GameOperation string

const (
	GameStateInvalid      GameState = "Invalid"
	GameStateInitializing GameState = "Initializing"
	GameStateStandingBy   GameState = "StandingBy"
	GameStateActive       GameState = "Active"
	GameStateTerminating  GameState = "Terminating"
	GameStateTerminated   GameState = "Terminated"
	GameStateQuarantined  GameState = "Quarantined" // Not used
)

const (
	GameOperationInvalid   GameOperation = "Invalid"
	GameOperationContinue  GameOperation = "Continue"
	GameOperationActive    GameOperation = "Active"
	GameOperationTerminate GameOperation = "Terminate"
)

// HeartbeatRequest contains data for the heartbeat request coming from the GSDK running alongside GameServer
type HeartbeatRequest struct {
	// CurrentGameState is the current state of the game server
	CurrentGameState GameState `json:"CurrentGameState"`
	// CurrentGameHealth is the current health of the game server
	CurrentGameHealth string `json:"CurrentGameHealth"`
	// CurrentPlayers is a slice containing details about the players currently connected to the game
	CurrentPlayers []ConnectedPlayer `json:"CurrentPlayers"`
}

// HeartbeatResponse contains data for the heartbeat response that is being sent to the GSDK running alongside GameServer
type HeartbeatResponse struct {
	SessionConfig               SessionConfig `json:"sessionConfig,omitempty"`
	NextScheduledMaintenanceUtc string        `json:"nextScheduledMaintenanceUtc,omitempty"`
	Operation                   GameOperation `json:"operation,omitempty"`
}

// SessionConfig contains data for the session config that is being sent to the GSDK running alongside GameServer
type SessionConfig struct {
	SessionId      string            `json:"sessionId,omitempty"`
	SessionCookie  string            `json:"sessionCookie,omitempty"`
	InitialPlayers []string          `json:"initialPlayers,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// ConnectedPlayer contains data for a player connected to the game
type ConnectedPlayer struct {
	PlayerId string
}

// SessionDetails contains data regarding the details for the session that occurs when the GameServer state changes from StandingBy to Active
type SessionDetails struct {
	SessionID      string
	SessionCookie  string
	InitialPlayers []string
	State          string
}
