package supabase

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

func q(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}

func nowISO(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// ----------------------------------------------------------- registration

func (c *Client) CheckUsername(ctx context.Context, username string) (bool, error) {
	var ok bool
	err := c.rpc(ctx, "check_username", map[string]any{"name": strings.ToLower(username)}, &ok)
	return ok, err
}

func (c *Client) Register(ctx context.Context, username, pubkey, inviteCode string) (model.User, error) {
	if _, err := c.uid(); err != nil {
		return model.User{}, err
	}
	args := map[string]any{"name": strings.ToLower(username), "pub": pubkey}
	if inviteCode != "" {
		args["invite"] = inviteCode
	} else {
		args["invite"] = nil
	}
	var r userRow
	if err := c.rpc(ctx, "register_username", args, &r); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "23505" && !strings.Contains(ae.Message, "already registered") {
			return model.User{}, fmt.Errorf("%w: %w", rendezvous.ErrUsernameTaken, ae)
		}
		return model.User{}, err
	}
	return r.model(), nil
}

func (c *Client) Me(ctx context.Context) (model.User, error) {
	id, err := c.uid()
	if err != nil {
		return model.User{}, err
	}
	return c.UserByID(ctx, id)
}

func (c *Client) Settings(ctx context.Context) (model.Settings, error) {
	var rows []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest/v1/instance_settings", q("select", "key,value"), nil, &rows); err != nil {
		return model.Settings{}, err
	}
	return settingsFromRows(rows)
}

// TouchLastSeen bumps users.last_seen for the caller.
func (c *Client) TouchLastSeen(ctx context.Context) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.rpc(ctx, "touch_last_seen", nil, nil)
}

// ------------------------------------------------------------------ users

func (c *Client) UserByName(ctx context.Context, username string) (model.User, error) {
	if _, err := c.uid(); err != nil {
		return model.User{}, err
	}
	var r userRow
	err := c.one(ctx, http.MethodGet, "/rest/v1/users",
		q("select", "*", "username", "eq."+strings.ToLower(username), "limit", "1"), nil, &r)
	return r.model(), err
}

func (c *Client) UserByID(ctx context.Context, id string) (model.User, error) {
	if _, err := c.uid(); err != nil {
		return model.User{}, err
	}
	var r userRow
	err := c.one(ctx, http.MethodGet, "/rest/v1/users", q("select", "*", "id", "eq."+id, "limit", "1"), nil, &r)
	return r.model(), err
}

func (c *Client) UsersByIDs(ctx context.Context, ids []string) ([]model.User, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []userRow
	err := c.do(ctx, http.MethodGet, "/rest/v1/users",
		q("select", "*", "id", "in.("+strings.Join(ids, ",")+")"), nil, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]model.User, len(rows))
	for i, r := range rows {
		out[i] = r.model()
	}
	return out, nil
}

// ListUsers returns every user. Only an admin device sees more than the
// caller's own row, so callers must not use it otherwise.
func (c *Client) ListUsers(ctx context.Context) ([]model.User, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	var rows []userRow
	err := c.do(ctx, http.MethodGet, "/rest/v1/users", q("select", "*", "order", "username.asc"), nil, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]model.User, len(rows))
	for i, r := range rows {
		out[i] = r.model()
	}
	return out, nil
}

func (c *Client) UpdatePubKey(ctx context.Context, pubkey string) error {
	me, err := c.uid()
	if err != nil {
		return err
	}
	var rows []userRow
	if err := c.do(ctx, http.MethodPatch, "/rest/v1/users", q("id", "eq."+me),
		map[string]any{"pubkey": pubkey}, &rows); err != nil {
		return err
	}
	if len(rows) == 0 {
		return rendezvous.ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- friends

func (c *Client) SendFriendRequest(ctx context.Context, toUserID string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.rpc(ctx, "send_friend_request", map[string]any{"target": toUserID}, nil)
}

func (c *Client) RespondFriendRequest(ctx context.Context, requesterID string, accept bool) error {
	me, err := c.uid()
	if err != nil {
		return err
	}
	status := model.FriendDenied
	if accept {
		status = model.FriendAccepted
	}
	var rows []friendshipRow
	err = c.do(ctx, http.MethodPatch, "/rest/v1/friendships",
		q("requester", "eq."+requesterID, "addressee", "eq."+me, "status", "eq.pending"),
		map[string]any{"status": status, "updated_at": nowISO(c.now())}, &rows)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return rendezvous.ErrNotFound
	}
	return nil
}

func (c *Client) RemoveFriend(ctx context.Context, userID string) error {
	me, err := c.uid()
	if err != nil {
		return err
	}
	filter := fmt.Sprintf("(and(requester.eq.%s,addressee.eq.%s),and(requester.eq.%s,addressee.eq.%s))", me, userID, userID, me)
	return c.do(ctx, http.MethodDelete, "/rest/v1/friendships", q("or", filter), nil, nil)
}

func (c *Client) ListFriendships(ctx context.Context) ([]model.Friendship, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	var rows []friendshipRow
	err := c.do(ctx, http.MethodGet, "/rest/v1/friendships", q("select", "*", "order", "created_at.asc"), nil, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]model.Friendship, len(rows))
	for i, r := range rows {
		out[i] = r.model()
	}
	return out, nil
}

// --------------------------------------------------------------- messages

func (c *Client) SendMessage(ctx context.Context, m model.Message) (model.Message, error) {
	if _, err := c.uid(); err != nil {
		return model.Message{}, err
	}
	epoch := m.KeyEpoch
	if epoch == 0 {
		epoch = 1
	}
	var r messageRow
	err := c.rpc(ctx, "send_message", map[string]any{
		"ct":     m.ConvType,
		"conv":   m.ConvID,
		"cipher": encodeBytea(m.Ciphertext),
		"n":      encodeBytea(m.Nonce),
		"epoch":  epoch,
	}, &r)
	if err != nil {
		return model.Message{}, err
	}
	return r.model(), nil
}

// ListMessages returns the newest page matching the query, oldest first.
func (c *Client) ListMessages(ctx context.Context, mq rendezvous.MessageQuery) ([]model.Message, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	limit := mq.Limit
	if limit <= 0 {
		limit = 50
	}
	v := q("select", "*", "conv_type", "eq."+string(mq.ConvType), "conv_id", "eq."+mq.ConvID,
		"order", "created_at.desc", "limit", strconv.Itoa(limit))
	if !mq.After.IsZero() {
		v.Add("created_at", "gt."+nowISO(mq.After))
	}
	if !mq.Before.IsZero() {
		v.Add("created_at", "lt."+nowISO(mq.Before))
	}
	var rows []messageRow
	if err := c.do(ctx, http.MethodGet, "/rest/v1/messages", v, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]model.Message, len(rows))
	for i, r := range rows {
		out[len(rows)-1-i] = r.model()
	}
	return out, nil
}

// ----------------------------------------------------------------- groups

func (c *Client) CreateGroup(ctx context.Context, name string, ownerWrappedKey []byte) (model.Group, error) {
	me, err := c.uid()
	if err != nil {
		return model.Group{}, err
	}
	var g groupRow
	err = c.one(ctx, http.MethodPost, "/rest/v1/groups", nil, map[string]any{"name": name, "owner": me}, &g)
	if err != nil {
		return model.Group{}, err
	}
	err = c.do(ctx, http.MethodPost, "/rest/v1/group_members", nil, map[string]any{
		"group_id": g.ID, "user_id": me, "wrapped_key": encodeBytea(ownerWrappedKey),
		"key_epoch": g.KeyEpoch, "wrapped_by": me,
	}, nil)
	if err != nil {
		return model.Group{}, err
	}
	return g.model(), nil
}

func (c *Client) ListGroups(ctx context.Context) ([]model.Group, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	var rows []groupRow
	if err := c.do(ctx, http.MethodGet, "/rest/v1/groups", q("select", "*", "order", "created_at.asc"), nil, &rows); err != nil {
		return nil, err
	}
	out := make([]model.Group, len(rows))
	for i, r := range rows {
		out[i] = r.model()
	}
	return out, nil
}

func (c *Client) ListGroupMembers(ctx context.Context, groupID string) ([]model.GroupMember, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	var rows []memberRow
	if err := c.do(ctx, http.MethodGet, "/rest/v1/group_members",
		q("select", "*", "group_id", "eq."+groupID, "order", "added_at.asc"), nil, &rows); err != nil {
		return nil, err
	}
	out := make([]model.GroupMember, len(rows))
	for i, r := range rows {
		out[i] = r.model()
	}
	return out, nil
}

func (c *Client) AddGroupMember(ctx context.Context, m model.GroupMember) error {
	me, err := c.uid()
	if err != nil {
		return err
	}
	by := m.WrappedBy
	if by == "" {
		by = me
	}
	epoch := m.KeyEpoch
	if epoch == 0 {
		epoch = 1
	}
	return c.do(ctx, http.MethodPost, "/rest/v1/group_members", nil, map[string]any{
		"group_id": m.GroupID, "user_id": m.UserID, "wrapped_key": encodeBytea(m.WrappedKey),
		"key_epoch": epoch, "wrapped_by": by,
	}, nil)
}

func (c *Client) RemoveGroupMember(ctx context.Context, groupID, userID string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/rest/v1/group_members",
		q("group_id", "eq."+groupID, "user_id", "eq."+userID), nil, nil)
}

// RotateGroupKey calls rotate_group_key; the RPC takes wrapped keys base64
// encoded inside its jsonb argument.
func (c *Client) RotateGroupKey(ctx context.Context, groupID string, epoch int, members []model.GroupMember) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	list := make([]map[string]any, 0, len(members))
	for _, m := range members {
		list = append(list, map[string]any{
			"user_id":     m.UserID,
			"wrapped_key": base64.StdEncoding.EncodeToString(m.WrappedKey),
		})
	}
	return c.rpc(ctx, "rotate_group_key", map[string]any{"g": groupID, "new_epoch": epoch, "members": list}, nil)
}

func (c *Client) DeleteGroup(ctx context.Context, groupID string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/rest/v1/groups", q("id", "eq."+groupID), nil, nil)
}

func (c *Client) TransferGroup(ctx context.Context, groupID, newOwner string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	var rows []groupRow
	if err := c.do(ctx, http.MethodPatch, "/rest/v1/groups", q("id", "eq."+groupID),
		map[string]any{"owner": newOwner}, &rows); err != nil {
		return err
	}
	if len(rows) == 0 {
		return rendezvous.ErrNotFound
	}
	return nil
}

// ----------------------------------------------------------------- grants

func (c *Client) PutGrant(ctx context.Context, g model.Grant) (model.Grant, error) {
	me, err := c.uid()
	if err != nil {
		return model.Grant{}, err
	}
	body := map[string]any{
		"grantor": me, "grantee": g.Grantee, "scope": g.Scope, "sudo": g.Sudo,
		"grantee_pubkey": g.GranteePubKey, "signature": encodeBytea(g.Signature),
	}
	if g.Sudo == "" {
		body["sudo"] = model.SudoBlocked
	}
	if g.ID != "" {
		body["id"] = g.ID
	}
	if !g.CreatedAt.IsZero() {
		body["created_at"] = nowISO(g.CreatedAt)
	}
	var r grantRow
	if err := c.one(ctx, http.MethodPost, "/rest/v1/grants", nil, body, &r); err != nil {
		return model.Grant{}, err
	}
	return r.model(), nil
}

func (c *Client) RevokeGrant(ctx context.Context, id string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	var rows []grantRow
	if err := c.do(ctx, http.MethodPatch, "/rest/v1/grants", q("id", "eq."+id, "revoked_at", "is.null"),
		map[string]any{"revoked_at": nowISO(c.now())}, &rows); err != nil {
		return err
	}
	if len(rows) == 0 {
		return rendezvous.ErrNotFound
	}
	return nil
}

func (c *Client) ListGrants(ctx context.Context) ([]model.Grant, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	var rows []grantRow
	if err := c.do(ctx, http.MethodGet, "/rest/v1/grants", q("select", "*", "order", "created_at.asc"), nil, &rows); err != nil {
		return nil, err
	}
	out := make([]model.Grant, len(rows))
	for i, r := range rows {
		out[i] = r.model()
	}
	return out, nil
}

// ----------------------------------------------------------- session logs

func (c *Client) LogSession(ctx context.Context, s model.SessionLog) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	body := map[string]any{"id": s.ID, "visitor": s.Visitor, "host": s.Host, "kind": s.Kind, "via": s.Via}
	if !s.StartedAt.IsZero() {
		body["started_at"] = nowISO(s.StartedAt)
	}
	if s.EndedAt != nil {
		body["ended_at"] = nowISO(*s.EndedAt)
	}
	return c.do(ctx, http.MethodPost, "/rest/v1/session_logs", nil, body, nil)
}

func (c *Client) EndSession(ctx context.Context, id string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, "/rest/v1/session_logs", q("id", "eq."+id),
		map[string]any{"ended_at": nowISO(c.now())}, nil)
}

// ------------------------------------------------------------------ admin

func (c *Client) SetSetting(ctx context.Context, key string, value any) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.rpc(ctx, "admin_set_setting", map[string]any{"k": key, "v": value}, nil)
}

func (c *Client) CreateInvite(ctx context.Context, uses int, expires *time.Time) (model.Invite, error) {
	if _, err := c.uid(); err != nil {
		return model.Invite{}, err
	}
	args := map[string]any{"uses": uses, "expires": nil}
	if expires != nil {
		args["expires"] = nowISO(*expires)
	}
	var r inviteRow
	if err := c.rpc(ctx, "admin_create_invite", args, &r); err != nil {
		return model.Invite{}, err
	}
	return r.model(), nil
}

func (c *Client) ListInvites(ctx context.Context) ([]model.Invite, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	var rows []inviteRow
	if err := c.do(ctx, http.MethodGet, "/rest/v1/invites", q("select", "*", "order", "created_at.desc"), nil, &rows); err != nil {
		return nil, err
	}
	out := make([]model.Invite, len(rows))
	for i, r := range rows {
		out[i] = r.model()
	}
	return out, nil
}

func (c *Client) RevokeInvite(ctx context.Context, code string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/rest/v1/invites", q("code", "eq."+code), nil, nil)
}

func (c *Client) SetUserFlag(ctx context.Context, userID, flag string, value bool) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	switch flag {
	case "banned", "muted", "approved":
	default:
		return fmt.Errorf("supabase: bad user flag %q", flag)
	}
	return c.rpc(ctx, "admin_set_user", map[string]any{"target": userID, "flag": flag, "val": value}, nil)
}

func (c *Client) SetUserRole(ctx context.Context, userID string, role model.Role) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.rpc(ctx, "admin_set_role", map[string]any{"target": userID, "r": role}, nil)
}

func (c *Client) RenameUser(ctx context.Context, userID, newName string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.rpc(ctx, "admin_rename", map[string]any{"target": userID, "newname": strings.ToLower(newName)}, nil)
}

func (c *Client) ResetUserKey(ctx context.Context, userID string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.rpc(ctx, "admin_reset_key", map[string]any{"target": userID}, nil)
}

func (c *Client) KickUser(ctx context.Context, userID string) error {
	if _, err := c.uid(); err != nil {
		return err
	}
	return c.rpc(ctx, "admin_kick", map[string]any{"target": userID}, nil)
}

func (c *Client) ListAdminLog(ctx context.Context, limit int) ([]model.AdminLogEntry, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	var rows []adminLogRow
	if err := c.do(ctx, http.MethodGet, "/rest/v1/admin_log",
		q("select", "*", "order", "at.desc", "limit", strconv.Itoa(limit)), nil, &rows); err != nil {
		return nil, err
	}
	out := make([]model.AdminLogEntry, len(rows))
	for i, r := range rows {
		out[i] = r.model()
	}
	return out, nil
}

// Stats calls admin_stats, which answers null for non-admins.
func (c *Client) Stats(ctx context.Context) (model.Stats, error) {
	if _, err := c.uid(); err != nil {
		return model.Stats{}, err
	}
	var raw json.RawMessage
	if err := c.rpc(ctx, "admin_stats", nil, &raw); err != nil {
		return model.Stats{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return model.Stats{}, rendezvous.ErrForbidden
	}
	var s model.Stats
	if err := json.Unmarshal(raw, &s); err != nil {
		return model.Stats{}, err
	}
	return s, nil
}

func (c *Client) ListAdminDevices(ctx context.Context) ([]model.AdminDevice, error) {
	if _, err := c.uid(); err != nil {
		return nil, err
	}
	var rows []adminDeviceRow
	if err := c.do(ctx, http.MethodGet, "/rest/v1/admin_devices", q("select", "*", "order", "created_at.asc"), nil, &rows); err != nil {
		return nil, err
	}
	out := make([]model.AdminDevice, len(rows))
	for i, r := range rows {
		out[i] = r.model()
	}
	return out, nil
}

func (c *Client) RegisterAdminDevice(ctx context.Context, deviceID, pubkey string) error {
	me, err := c.uid()
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/rest/v1/admin_devices", nil,
		map[string]any{"device_id": deviceID, "user_id": me, "pubkey": pubkey}, nil)
}

// ApproveAdminDevice approves a pending device. The token the server mints
// for that device is returned by ApproveAdminDeviceToken; this wrapper
// discards it to satisfy the Backend interface.
func (c *Client) ApproveAdminDevice(ctx context.Context, deviceID string) error {
	_, err := c.ApproveAdminDeviceToken(ctx, deviceID)
	return err
}

// ApproveAdminDeviceToken approves deviceID and returns its admin token,
// which must be handed to that device out of band.
func (c *Client) ApproveAdminDeviceToken(ctx context.Context, deviceID string) (string, error) {
	if _, err := c.uid(); err != nil {
		return "", err
	}
	var tok string
	if err := c.rpc(ctx, "admin_approve_device", map[string]any{"dev": deviceID}, &tok); err != nil {
		return "", err
	}
	return tok, nil
}

// BootstrapAdminDevice self-approves the very first admin device and
// installs the returned token on this client.
func (c *Client) BootstrapAdminDevice(ctx context.Context, deviceID, pubkey string) (string, error) {
	if _, err := c.uid(); err != nil {
		return "", err
	}
	var tok string
	if err := c.rpc(ctx, "admin_bootstrap_device", map[string]any{"dev": deviceID, "pub": pubkey}, &tok); err != nil {
		return "", err
	}
	if tok == "" {
		return "", errors.New("supabase: bootstrap returned no token")
	}
	c.SetAdminToken(tok)
	return tok, nil
}

// --------------------------------------------------------------- signals

func (c *Client) SendSignal(ctx context.Context, s rendezvous.Signal) error {
	me, err := c.uid()
	if err != nil {
		return err
	}
	from := s.From
	if from == "" {
		from = me
	}
	return c.do(ctx, http.MethodPost, "/rest/v1/signals", nil, map[string]any{
		"session_id": s.SessionID, "from_user": from, "to_user": s.To, "type": s.Type,
		"payload": encodeBytea(s.Payload),
	}, nil)
}

func (c *Client) deleteSignal(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, "/rest/v1/signals", q("id", "eq."+strconv.FormatInt(id, 10)), nil, nil)
}
