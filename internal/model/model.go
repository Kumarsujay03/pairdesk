// Package model holds the plain data types shared across PairDesk. They
// mirror the database rows but carry no behaviour.
package model

import "time"

type Role string

const (
	RoleUser   Role = "user"
	RoleHelper Role = "helper"
	RoleAdmin  Role = "admin"
)

type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	PubKey    string    `json:"pubkey"`
	Role      Role      `json:"role"`
	Banned    bool      `json:"banned"`
	Muted     bool      `json:"muted"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	Approved  bool      `json:"approved"`
	KickedAt  time.Time `json:"kicked_at"`
}

type FriendStatus string

const (
	FriendPending  FriendStatus = "pending"
	FriendAccepted FriendStatus = "accepted"
	FriendDenied   FriendStatus = "denied"
)

type Friendship struct {
	Requester string       `json:"requester"`
	Addressee string       `json:"addressee"`
	Status    FriendStatus `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// Other returns the id of the party that is not me.
func (f Friendship) Other(me string) string {
	if f.Requester == me {
		return f.Addressee
	}
	return f.Requester
}

type ConvType string

const (
	ConvDM    ConvType = "dm"
	ConvGroup ConvType = "group"
)

// Message is always stored encrypted. ConvID for a DM is the sorted pair
// "a:b" of user ids; for a group it is the group id.
type Message struct {
	ID         string    `json:"id"`
	ConvType   ConvType  `json:"conv_type"`
	ConvID     string    `json:"conv_id"`
	Sender     string    `json:"sender"`
	Ciphertext []byte    `json:"ciphertext"`
	Nonce      []byte    `json:"nonce"`
	KeyEpoch   int       `json:"key_epoch"`
	CreatedAt  time.Time `json:"created_at"`
}

// DMConvID builds the canonical DM conversation id.
func DMConvID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + ":" + b
}

type Group struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	KeyEpoch  int       `json:"key_epoch"`
	CreatedAt time.Time `json:"created_at"`
}

type GroupMember struct {
	GroupID    string `json:"group_id"`
	UserID     string `json:"user_id"`
	WrappedKey []byte `json:"wrapped_key"`
	KeyEpoch   int    `json:"key_epoch"`
	WrappedBy  string `json:"wrapped_by"`
}

type GrantScope string

const (
	ScopeWatch GrantScope = "watch"
	ScopeBoard GrantScope = "board"
)

type SudoTier string

const (
	SudoBlocked SudoTier = "blocked"
	SudoAsk     SudoTier = "ask"
	SudoAuto    SudoTier = "auto"
)

// Grant is a standing permission the grantor gives the grantee. The
// signature covers Grantor|Grantee|Scope|Sudo|CreatedAt|GranteePubKey and is
// made with the grantor's device key. Clients enforce from their own local
// signed copy; the database row is a hint.
type Grant struct {
	ID            string     `json:"id"`
	Grantor       string     `json:"grantor"`
	Grantee       string     `json:"grantee"`
	Scope         GrantScope `json:"scope"`
	Sudo          SudoTier   `json:"sudo"`
	GranteePubKey string     `json:"grantee_pubkey"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	Signature     []byte     `json:"signature"`
}

type SessionKind string

const (
	SessionWatch  SessionKind = "watch"
	SessionBoard  SessionKind = "board"
	SessionScreen SessionKind = "screen"
	SessionShare  SessionKind = "share"
	SessionSend   SessionKind = "send"
)

type SessionVia string

const (
	ViaGrant SessionVia = "grant"
	ViaCard  SessionVia = "card"
)

type SessionLog struct {
	ID        string      `json:"id"`
	Visitor   string      `json:"visitor"`
	Host      string      `json:"host"`
	Kind      SessionKind `json:"kind"`
	Via       SessionVia  `json:"via"`
	StartedAt time.Time   `json:"started_at"`
	EndedAt   *time.Time  `json:"ended_at,omitempty"`
}

type RegistrationMode string

const (
	RegOpen   RegistrationMode = "open"
	RegClosed RegistrationMode = "closed"
	RegInvite RegistrationMode = "invite"
)

// Settings is the instance_settings table flattened.
type Settings struct {
	Registration    RegistrationMode `json:"registration"`
	RequireApproval bool             `json:"require_approval"`
	UserCap         int              `json:"user_cap"`
	MinVersion      string           `json:"min_version"`
	Maintenance     bool             `json:"maintenance"`
	MaintenanceMsg  string           `json:"maintenance_msg"`
	Announcement    string           `json:"announcement"`
	AnnouncementID  string           `json:"announcement_id"`
	MsgsPerMin      int              `json:"msgs_per_min"`
	AdminPubKey     string           `json:"admin_pubkey"`
}

type Invite struct {
	Code      string     `json:"code"`
	UsesLeft  int        `json:"uses_left"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
}

type AdminDevice struct {
	DeviceID   string    `json:"device_id"`
	PubKey     string    `json:"pubkey"`
	Approved   bool      `json:"approved"`
	ApprovedAt time.Time `json:"approved_at"`
}

type AdminLogEntry struct {
	ID     int64     `json:"id"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

type Stats struct {
	UsersTotal   int `json:"users_total"`
	UsersOnline  int `json:"users_online"`
	UsersToday   int `json:"users_today"`
	MsgsToday    int `json:"msgs_today"`
	SessionsWeek int `json:"sessions_week"`
	DBBytes      int64 `json:"db_bytes"`
}
