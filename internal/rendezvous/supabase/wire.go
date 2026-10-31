package supabase

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// bytea is a PostgreSQL bytea column on the wire: PostgREST renders it as
// the hex string "\x..." and accepts the same form back.
type bytea []byte

func (b bytea) MarshalJSON() ([]byte, error) {
	return json.Marshal(encodeBytea(b))
}

func (b *bytea) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*b = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, err := decodeBytea(s)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

func encodeBytea(b []byte) string { return `\x` + hex.EncodeToString(b) }

// decodeBytea accepts PostgreSQL hex form and, leniently, base64.
func decodeBytea(s string) ([]byte, error) {
	if strings.HasPrefix(s, `\x`) || strings.HasPrefix(s, `\X`) {
		v, err := hex.DecodeString(s[2:])
		if err != nil {
			return nil, fmt.Errorf("bytea: %w", err)
		}
		return v, nil
	}
	if s == "" {
		return []byte{}, nil
	}
	if v, err := base64.StdEncoding.DecodeString(s); err == nil {
		return v, nil
	}
	return nil, fmt.Errorf("bytea: unrecognised encoding")
}

// ts is a timestamptz that tolerates the slightly different renderings
// PostgREST and the realtime server use.
type ts struct{ time.Time }

var tsLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
}

func (t *ts) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	for _, l := range tsLayouts {
		if v, err := time.Parse(l, s); err == nil {
			t.Time = v
			return nil
		}
	}
	return fmt.Errorf("timestamp: cannot parse %q", s)
}

func (t ts) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.Time.UTC().Format(time.RFC3339Nano))
}

func tsPtr(t *ts) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.Time
	return &v
}

// --- rows ---

type userRow struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	PubKey    string `json:"pubkey"`
	Role      string `json:"role"`
	Banned    bool   `json:"banned"`
	Muted     bool   `json:"muted"`
	Approved  bool   `json:"approved"`
	CreatedAt ts     `json:"created_at"`
	LastSeen  ts     `json:"last_seen"`
}

func (r userRow) model() model.User {
	return model.User{
		ID: r.ID, Username: r.Username, PubKey: r.PubKey, Role: model.Role(r.Role),
		Banned: r.Banned, Muted: r.Muted, Approved: r.Approved,
		CreatedAt: r.CreatedAt.Time, LastSeen: r.LastSeen.Time,
	}
}

type friendshipRow struct {
	Requester string `json:"requester"`
	Addressee string `json:"addressee"`
	Status    string `json:"status"`
	CreatedAt ts     `json:"created_at"`
	UpdatedAt ts     `json:"updated_at"`
}

func (r friendshipRow) model() model.Friendship {
	return model.Friendship{
		Requester: r.Requester, Addressee: r.Addressee, Status: model.FriendStatus(r.Status),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

type groupRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	KeyEpoch  int    `json:"key_epoch"`
	CreatedAt ts     `json:"created_at"`
}

func (r groupRow) model() model.Group {
	return model.Group{ID: r.ID, Name: r.Name, Owner: r.Owner, KeyEpoch: r.KeyEpoch, CreatedAt: r.CreatedAt.Time}
}

type memberRow struct {
	GroupID    string `json:"group_id"`
	UserID     string `json:"user_id"`
	WrappedKey bytea  `json:"wrapped_key"`
	KeyEpoch   int    `json:"key_epoch"`
	WrappedBy  string `json:"wrapped_by"`
}

func (r memberRow) model() model.GroupMember {
	return model.GroupMember{GroupID: r.GroupID, UserID: r.UserID, WrappedKey: []byte(r.WrappedKey), KeyEpoch: r.KeyEpoch, WrappedBy: r.WrappedBy}
}

type messageRow struct {
	ID         string `json:"id"`
	ConvType   string `json:"conv_type"`
	ConvID     string `json:"conv_id"`
	Sender     string `json:"sender"`
	Ciphertext bytea  `json:"ciphertext"`
	Nonce      bytea  `json:"nonce"`
	KeyEpoch   int    `json:"key_epoch"`
	CreatedAt  ts     `json:"created_at"`
}

func (r messageRow) model() model.Message {
	return model.Message{
		ID: r.ID, ConvType: model.ConvType(r.ConvType), ConvID: r.ConvID, Sender: r.Sender,
		Ciphertext: []byte(r.Ciphertext), Nonce: []byte(r.Nonce), KeyEpoch: r.KeyEpoch, CreatedAt: r.CreatedAt.Time,
	}
}

type grantRow struct {
	ID            string `json:"id,omitempty"`
	Grantor       string `json:"grantor"`
	Grantee       string `json:"grantee"`
	Scope         string `json:"scope"`
	Sudo          string `json:"sudo"`
	GranteePubKey string `json:"grantee_pubkey"`
	Signature     bytea  `json:"signature"`
	CreatedAt     ts     `json:"created_at"`
	RevokedAt     *ts    `json:"revoked_at"`
}

func (r grantRow) model() model.Grant {
	return model.Grant{
		ID: r.ID, Grantor: r.Grantor, Grantee: r.Grantee, Scope: model.GrantScope(r.Scope),
		Sudo: model.SudoTier(r.Sudo), GranteePubKey: r.GranteePubKey, Signature: []byte(r.Signature),
		CreatedAt: r.CreatedAt.Time, RevokedAt: tsPtr(r.RevokedAt),
	}
}

type signalRow struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	FromUser  string `json:"from_user"`
	ToUser    string `json:"to_user"`
	Type      string `json:"type"`
	Payload   bytea  `json:"payload"`
}

func (r signalRow) model() rendezvous.Signal {
	return rendezvous.Signal{SessionID: r.SessionID, From: r.FromUser, To: r.ToUser, Type: r.Type, Payload: []byte(r.Payload)}
}

type inviteRow struct {
	Code      string `json:"code"`
	UsesLeft  int    `json:"uses_left"`
	ExpiresAt *ts    `json:"expires_at"`
	CreatedBy string `json:"created_by"`
	CreatedAt ts     `json:"created_at"`
}

func (r inviteRow) model() model.Invite {
	return model.Invite{Code: r.Code, UsesLeft: r.UsesLeft, ExpiresAt: tsPtr(r.ExpiresAt), CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt.Time}
}

type adminDeviceRow struct {
	DeviceID   string `json:"device_id"`
	PubKey     string `json:"pubkey"`
	Approved   bool   `json:"approved"`
	ApprovedAt *ts    `json:"approved_at"`
}

func (r adminDeviceRow) model() model.AdminDevice {
	d := model.AdminDevice{DeviceID: r.DeviceID, PubKey: r.PubKey, Approved: r.Approved}
	if r.ApprovedAt != nil {
		d.ApprovedAt = r.ApprovedAt.Time
	}
	return d
}

type adminLogRow struct {
	ID     int64   `json:"id"`
	Actor  string  `json:"actor"`
	Action string  `json:"action"`
	Target *string `json:"target"`
	Detail *string `json:"detail"`
	At     ts      `json:"at"`
}

func (r adminLogRow) model() model.AdminLogEntry {
	e := model.AdminLogEntry{ID: r.ID, Actor: r.Actor, Action: r.Action, At: r.At.Time}
	if r.Target != nil {
		e.Target = *r.Target
	}
	if r.Detail != nil {
		e.Detail = *r.Detail
	}
	return e
}

// settingsFromRows flattens instance_settings rows into model.Settings.
func settingsFromRows(rows []struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}) (model.Settings, error) {
	obj := make(map[string]json.RawMessage, len(rows))
	for _, r := range rows {
		obj[r.Key] = r.Value
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return model.Settings{}, err
	}
	var s model.Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return model.Settings{}, fmt.Errorf("settings: %w", err)
	}
	return s, nil
}
