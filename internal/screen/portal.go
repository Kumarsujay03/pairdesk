package screen

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	portalDest      = "org.freedesktop.portal.Desktop"
	portalPath      = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	screenCastIface = "org.freedesktop.portal.ScreenCast"
	requestIface    = "org.freedesktop.portal.Request"
	sessionIface    = "org.freedesktop.portal.Session"

	sourceTypeMonitor  uint32 = 1
	cursorModeEmbedded uint32 = 2
)

// portalSession is an open org.freedesktop.portal.ScreenCast session.
type portalSession struct {
	conn    *dbus.Conn
	session dbus.ObjectPath
	Node    uint32   // PipeWire node id of the selected monitor stream
	Remote  *os.File // PipeWire remote fd from OpenPipeWireRemote
	Width   int32    // stream size reported by the portal, 0 if unknown
	Height  int32
}

// ErrPortalUnavailable wraps failures to reach the desktop portal so the
// caller can decide to fall back to X11 capture.
var ErrPortalUnavailable = errors.New("screen: desktop portal unavailable")

// openPortalSession walks the ScreenCast portal flow:
// CreateSession -> SelectSources -> Start -> OpenPipeWireRemote.
// Every call is bounded by ctx; the caller should set a deadline because
// Start shows a chooser dialog that the user may ignore.
func openPortalSession(ctx context.Context) (*portalSession, error) {
	conn, err := dbus.ConnectSessionBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPortalUnavailable, err)
	}
	if !conn.SupportsUnixFDs() {
		conn.Close()
		return nil, fmt.Errorf("%w: session bus does not pass unix fds", ErrPortalUnavailable)
	}
	ps := &portalSession{conn: conn}
	if err := ps.run(ctx); err != nil {
		ps.Close()
		return nil, err
	}
	return ps, nil
}

func (ps *portalSession) run(ctx context.Context) error {
	obj := ps.conn.Object(portalDest, portalPath)
	sender := strings.TrimPrefix(ps.conn.Names()[0], ":")
	sender = strings.ReplaceAll(sender, ".", "_")

	// Fail fast with a distinguishable error if the portal or interface is
	// missing (typical on bare X11 sessions).
	var version uint32
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, screenCastIface, "version").Store(&version); err != nil {
		return fmt.Errorf("%w: %v", ErrPortalUnavailable, err)
	}

	// CreateSession
	sessionToken := newToken()
	results, err := ps.request(ctx, sender, func(reqToken string) *dbus.Call {
		return obj.CallWithContext(ctx, screenCastIface+".CreateSession", 0, map[string]dbus.Variant{
			"handle_token":         dbus.MakeVariant(reqToken),
			"session_handle_token": dbus.MakeVariant(sessionToken),
		})
	})
	if err != nil {
		return fmt.Errorf("screen: portal CreateSession: %w", err)
	}
	if v, ok := results["session_handle"]; ok {
		switch h := v.Value().(type) {
		case string:
			ps.session = dbus.ObjectPath(h)
		case dbus.ObjectPath:
			ps.session = h
		}
	}
	if ps.session == "" {
		ps.session = dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/portal/desktop/session/%s/%s", sender, sessionToken))
	}

	// SelectSources: one monitor, cursor embedded in the frames.
	if _, err := ps.request(ctx, sender, func(reqToken string) *dbus.Call {
		return obj.CallWithContext(ctx, screenCastIface+".SelectSources", 0, ps.session, map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(reqToken),
			"types":        dbus.MakeVariant(sourceTypeMonitor),
			"multiple":     dbus.MakeVariant(false),
			"cursor_mode":  dbus.MakeVariant(cursorModeEmbedded),
		})
	}); err != nil {
		return fmt.Errorf("screen: portal SelectSources: %w", err)
	}

	// Start: this is where the compositor shows its picker.
	results, err = ps.request(ctx, sender, func(reqToken string) *dbus.Call {
		return obj.CallWithContext(ctx, screenCastIface+".Start", 0, ps.session, "", map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(reqToken),
		})
	})
	if err != nil {
		return fmt.Errorf("screen: portal Start: %w", err)
	}
	streamsV, ok := results["streams"]
	if !ok {
		return errors.New("screen: portal Start returned no streams")
	}
	var streams []struct {
		Node  uint32
		Props map[string]dbus.Variant
	}
	if err := dbus.Store([]interface{}{streamsV.Value()}, &streams); err != nil {
		return fmt.Errorf("screen: portal Start: decoding streams: %w", err)
	}
	if len(streams) == 0 {
		return errors.New("screen: portal Start returned an empty stream list")
	}
	ps.Node = streams[0].Node
	if sz, ok := streams[0].Props["size"]; ok {
		var wh struct{ W, H int32 }
		if dbus.Store([]interface{}{sz.Value()}, &wh) == nil {
			ps.Width, ps.Height = wh.W, wh.H
		}
	}

	// OpenPipeWireRemote
	var fd dbus.UnixFD
	if err := obj.CallWithContext(ctx, screenCastIface+".OpenPipeWireRemote", 0, ps.session, map[string]dbus.Variant{}).Store(&fd); err != nil {
		return fmt.Errorf("screen: portal OpenPipeWireRemote: %w", err)
	}
	ps.Remote = os.NewFile(uintptr(fd), "pipewire-remote")
	return nil
}

// request subscribes to the Response signal of the request object the portal
// will create, performs the call, and waits for the response. Portal requests
// use a predictable object path derived from our unique name and a token so
// we can subscribe before the call and never miss the signal.
func (ps *portalSession) request(ctx context.Context, sender string, call func(reqToken string) *dbus.Call) (map[string]dbus.Variant, error) {
	token := newToken()
	reqPath := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/portal/desktop/request/%s/%s", sender, token))

	matchOpts := []dbus.MatchOption{
		dbus.WithMatchObjectPath(reqPath),
		dbus.WithMatchInterface(requestIface),
		dbus.WithMatchMember("Response"),
	}
	if err := ps.conn.AddMatchSignalContext(ctx, matchOpts...); err != nil {
		return nil, err
	}
	defer func() { _ = ps.conn.RemoveMatchSignal(matchOpts...) }()

	sigs := make(chan *dbus.Signal, 8)
	ps.conn.Signal(sigs)
	defer ps.conn.RemoveSignal(sigs)

	c := call(token)
	var returnedPath dbus.ObjectPath
	if err := c.Store(&returnedPath); err != nil {
		return nil, err
	}
	// Older portals may ignore handle_token and return their own path.
	if returnedPath != reqPath {
		extra := []dbus.MatchOption{
			dbus.WithMatchObjectPath(returnedPath),
			dbus.WithMatchInterface(requestIface),
			dbus.WithMatchMember("Response"),
		}
		if err := ps.conn.AddMatchSignalContext(ctx, extra...); err != nil {
			return nil, err
		}
		defer func() { _ = ps.conn.RemoveMatchSignal(extra...) }()
	}

	for {
		select {
		case <-ctx.Done():
			_ = ps.conn.Object(portalDest, returnedPath).Call(requestIface+".Close", 0)
			return nil, ctx.Err()
		case sig, ok := <-sigs:
			if !ok {
				return nil, errors.New("dbus connection closed")
			}
			if sig.Path != reqPath && sig.Path != returnedPath {
				continue
			}
			if sig.Name != requestIface+".Response" || len(sig.Body) < 2 {
				continue
			}
			code, _ := sig.Body[0].(uint32)
			res, _ := sig.Body[1].(map[string]dbus.Variant)
			switch code {
			case 0:
				return res, nil
			case 1:
				return nil, errors.New("cancelled by user")
			default:
				return nil, fmt.Errorf("portal response code %d", code)
			}
		}
	}
}

// Close ends the portal session (Session.Close) and releases the PipeWire fd
// and bus connection.
func (ps *portalSession) Close() error {
	var first error
	if ps.session != "" && ps.conn != nil {
		if call := ps.conn.Object(portalDest, ps.session).Call(sessionIface+".Close", 0); call.Err != nil {
			first = call.Err
		}
		ps.session = ""
	}
	if ps.Remote != nil {
		_ = ps.Remote.Close()
		ps.Remote = nil
	}
	if ps.conn != nil {
		_ = ps.conn.Close()
		ps.conn = nil
	}
	return first
}

func newToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "pairdesk_" + hex.EncodeToString(b[:])
}
