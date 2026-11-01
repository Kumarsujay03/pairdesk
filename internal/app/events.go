package app

import "github.com/Kumarsujay03/pairdesk/internal/model"

// EventKind enumerates what the UI can be told.
type EventKind string

const (
	EvPhase          EventKind = "phase"
	EvKeyForged      EventKind = "key_forged"
	EvMessage        EventKind = "message"       // Msg set
	EvFriendRequest  EventKind = "friend_request" // UserID = requester
	EvFriendAccepted EventKind = "friend_accepted"
	EvFriendRemoved  EventKind = "friend_removed"
	EvPresence       EventKind = "presence"
	EvGroupChanged   EventKind = "group_changed"
	EvGroupInvite    EventKind = "group_invite"
	EvGrantChanged   EventKind = "grant_changed"
	EvSettings       EventKind = "settings"
	EvAnnouncement   EventKind = "announcement"
	EvKeyChanged     EventKind = "key_changed" // UserID + Text=username
	EvNotice         EventKind = "notice"
	EvBootStep       EventKind = "boot_step" // Text = what startup is doing now
	EvError          EventKind = "error"
	EvTyping         EventKind = "typing"
	EvCard           EventKind = "card" // Card set: an access / sudo / share request needing a decision
	EvSession        EventKind = "session"
	EvDisconnected   EventKind = "disconnected"
	EvReconnected    EventKind = "reconnected"
	EvKicked         EventKind = "kicked"
	EvBanned         EventKind = "banned"
)

// Event is what the UI consumes.
type Event struct {
	Kind    EventKind
	Phase   Phase
	Msg     *Msg
	UserID  string
	Online  bool
	GroupID string
	Text    string
	Card    *Card
	Session *SessionInfo
	Err     error
}

// Msg is a decrypted message.
type Msg struct {
	ID        string
	ConvType  model.ConvType
	ConvID    string
	Sender    string
	Text      string
	Kind      string // "text" | "reaction" | "me" | "system" | "file"
	ReactsTo  string
	At        int64 // unix nanos
	Mine      bool
	Decrypted bool
}

// Card is a decision the local user must make.
type Card struct {
	ID        string
	Kind      string // access | sudo | share | file | friend | group | device
	From      string // user id
	FromName  string
	Text      string
	Session   string
	Scope     model.SessionKind
	Control   bool
	Command   string
	ExpiresAt int64
	Reply     func(accept bool, sudo model.SudoTier)
	// ReplyWithPassword is set on sudo cards in the blocked tier: the host
	// approves one command and supplies their own password locally. The
	// password goes straight to sudo on this machine; it is never sent.
	ReplyWithPassword func(accept bool, password string)
	// Progress is set on file cards once accepted (0..1).
	Progress float64
}

// SessionInfo describes a live or finished session for the UI.
type SessionInfo struct {
	ID       string
	Kind     model.SessionKind
	Peer     string
	PeerName string
	Host     bool // true if this machine is the host
	Via      model.SessionVia
	State    string // connecting | live | ended
	Error    string
}
