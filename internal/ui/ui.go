// Package ui is the graphical front door: one small window, no
// jargon. Front door (Host / Join) -> setup (name + password, or
// code + password) -> the room view with the big code and the
// member list. It drives the real core — the same node and DHT
// service the command-line harness uses; nothing here is a mock.
package ui

import (
	"fmt"
	"image"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"svlan/internal/adapter"
	"svlan/internal/dhtx"
	"svlan/internal/node"
	"svlan/internal/room"
)

type screen int

const (
	screenFront screen = iota
	screenHostSetup
	screenJoinSetup
	screenRoom
)

type liveRoom struct {
	nd       *node.Node
	dht      *dhtx.Service
	hosting  bool
	code     string
	joinedAt time.Time
	testMode bool // no real adapter on this computer (verification VMs)
}

type state struct {
	w  *app.Window
	th *material.Theme

	screen   screen
	status   string // plain-language message for the current screen
	starting bool

	nameEd     widget.Editor
	hostPwEd   widget.Editor
	joinCodeEd widget.Editor
	joinPwEd   widget.Editor

	hostBtn     widget.Clickable
	joinBtn     widget.Clickable
	startBtn    widget.Clickable
	joinRoomBtn widget.Clickable
	backBtn     widget.Clickable
	closeBtn    widget.Clickable

	mu   sync.Mutex
	live *liveRoom
}

// Run starts the graphical application (blocking). Called on the
// main goroutine, as Gio requires.
func Run() {
	w := new(app.Window)
	w.Option(app.Title("Room Link"), app.Size(unit.Dp(430), unit.Dp(600)))
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	s := &state{w: w, th: th}
	for _, ed := range []*widget.Editor{&s.nameEd, &s.hostPwEd, &s.joinCodeEd, &s.joinPwEd} {
		ed.SingleLine = true
	}
	s.hostPwEd.Mask = '*'
	s.joinPwEd.Mask = '*'
	s.nameEd.SetText(defaultName())

	// Keep pings and the member list fresh.
	go func() {
		for range time.Tick(time.Second) {
			w.Invalidate()
		}
	}()

	var ops op.Ops
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			s.closeRoom()
			if e.Err != nil {
				fmt.Fprintln(os.Stderr, e.Err)
				os.Exit(1)
			}
			return
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			s.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func defaultName() string {
	if u, err := os.UserHomeDir(); err == nil && u != "" {
		// A friendly default the user can overwrite.
		parts := strings.Split(strings.Trim(u, "/"), "/")
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			return parts[len(parts)-1]
		}
	}
	return "Player"
}

// --- Room lifecycle ------------------------------------------------

func (s *state) startRoom(hosting bool) {
	name := strings.TrimSpace(s.nameEd.Text())
	if name == "" {
		name = "Player"
	}
	var code, password string
	if hosting {
		password = s.hostPwEd.Text()
		if password == "" {
			s.status = "Set a room password first — your friends will need it to get in."
			return
		}
		c, err := room.GenerateCode()
		if err != nil {
			s.status = "Something went wrong starting the room. Please try again."
			return
		}
		code = c
	} else {
		code = s.joinCodeEd.Text()
		if !room.ValidCode(code) {
			s.status = "That code doesn't look right — it should look like ABCDE-FGHJK."
			return
		}
		password = s.joinPwEd.Text()
		if password == "" {
			s.status = "Enter the room password the host gave you."
			return
		}
	}
	s.starting = true
	s.status = "Starting…"
	go func() {
		live, err := openRoom(name, code, password, hosting)
		s.mu.Lock()
		s.starting = false
		if err != nil {
			s.status = "Couldn't start — please check your connection and try again."
			s.mu.Unlock()
			s.w.Invalidate()
			return
		}
		s.live = live
		s.mu.Unlock()
		s.screen = screenRoom
		s.status = ""
		s.w.Invalidate()
	}()
}

// openRoom builds a real node, tries for a real adapter (falling
// back to the loopback test adapter where none can be created),
// and starts the serverless lookup in the background.
func openRoom(name, code, password string, hosting bool) (*liveRoom, error) {
	loop := adapter.NewLoopback("ui-" + name)
	nd, err := node.New(node.Config{
		Name:    name,
		RoomKey: room.RoomKey(code, password),
		Adapter: loop,
		Log:     func(string, ...any) {},
	})
	if err != nil {
		return nil, err
	}
	live := &liveRoom{hosting: hosting, code: code, joinedAt: time.Now(), testMode: true}
	if ip, ok := room.ParseIP(nd.SelfIP()); ok {
		if tunAdp, terr := adapter.OpenTUN("svlan0", ip); terr == nil {
			nd.SetAdapter(tunAdp)
			live.testMode = false
		}
	}
	if err := nd.Start(); err != nil {
		return nil, err
	}
	live.nd = nd
	if svc, err := dhtx.Start(0, nil); err == nil {
		live.dht = svc
		tunnelPort := portOf(nd.ListenAddr())
		go svc.AnnounceLoop(room.DHTKey(code), tunnelPort, 30*time.Second, nil, func(addr string) {
			if portOf(addr) == tunnelPort && isLocal(addr) {
				return // our own listing coming back at us
			}
			nd.Connect(addr)
		})
	}
	return live, nil
}

func (s *state) closeRoom() {
	s.mu.Lock()
	live := s.live
	s.live = nil
	s.mu.Unlock()
	if live != nil {
		if live.dht != nil {
			live.dht.Close()
		}
		live.nd.Close()
	}
}

func portOf(addr string) int {
	_, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return -1
	}
	p, _ := strconv.Atoi(ps)
	return p
}

func isLocal(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// --- Layout ---------------------------------------------------------

func (s *state) layout(gtx layout.Context) {
	paint.FillShape(gtx.Ops, s.th.Bg, clip.Rect{Max: gtx.Constraints.Max}.Op())
	inset := layout.UniformInset(unit.Dp(20))
	inset.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		switch s.screen {
		case screenFront:
			return s.layoutFront(gtx)
		case screenHostSetup:
			return s.layoutHostSetup(gtx)
		case screenJoinSetup:
			return s.layoutJoinSetup(gtx)
		case screenRoom:
			return s.layoutRoom(gtx)
		}
		return layout.Dimensions{}
	})
}

func (s *state) layoutFront(gtx layout.Context) layout.Dimensions {
	if s.hostBtn.Clicked(gtx) {
		s.screen = screenHostSetup
		s.status = ""
	}
	if s.joinBtn.Clicked(gtx) {
		s.screen = screenJoinSetup
		s.status = ""
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.H4(s.th, "Play together, from anywhere").Layout),
		layout.Rigid(spacer(6)),
		layout.Rigid(material.Body2(s.th, "Connect your computers like you're all in the same room. No accounts, no sign-ups — just a code and a password.").Layout),
		layout.Rigid(spacer(28)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.Button(s.th, &s.hostBtn, "Host a Room").Layout(gtx)
		}),
		layout.Rigid(spacer(12)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.Button(s.th, &s.joinBtn, "Join a Room").Layout(gtx)
		}),
	)
}

func (s *state) layoutHostSetup(gtx layout.Context) layout.Dimensions {
	if s.backBtn.Clicked(gtx) {
		s.screen = screenFront
		s.status = ""
	}
	if s.startBtn.Clicked(gtx) && !s.starting {
		s.startRoom(true)
	}
	children := []layout.FlexChild{
		layout.Rigid(material.H5(s.th, "Host a room").Layout),
		layout.Rigid(spacer(16)),
		layout.Rigid(material.Body2(s.th, "Your name").Layout),
		layout.Rigid(s.editor(&s.nameEd, "e.g. Alex")),
		layout.Rigid(spacer(12)),
		layout.Rigid(material.Body2(s.th, "Room password — friends will need this too").Layout),
		layout.Rigid(s.editor(&s.hostPwEd, "Pick a password")),
		layout.Rigid(spacer(20)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "Start Hosting"
			if s.starting {
				label = "Starting…"
			}
			return material.Button(s.th, &s.startBtn, label).Layout(gtx)
		}),
		layout.Rigid(spacer(10)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.Button(s.th, &s.backBtn, "Back").Layout(gtx)
		}),
	}
	if s.status != "" {
		children = append(children, layout.Rigid(spacer(12)), layout.Rigid(material.Body2(s.th, s.status).Layout))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (s *state) layoutJoinSetup(gtx layout.Context) layout.Dimensions {
	if s.backBtn.Clicked(gtx) {
		s.screen = screenFront
		s.status = ""
	}
	if s.joinRoomBtn.Clicked(gtx) && !s.starting {
		s.startRoom(false)
	}
	children := []layout.FlexChild{
		layout.Rigid(material.H5(s.th, "Join a room").Layout),
		layout.Rigid(spacer(16)),
		layout.Rigid(material.Body2(s.th, "Your name").Layout),
		layout.Rigid(s.editor(&s.nameEd, "e.g. Alex")),
		layout.Rigid(spacer(12)),
		layout.Rigid(material.Body2(s.th, "Room code").Layout),
		layout.Rigid(s.editor(&s.joinCodeEd, "ABCDE-FGHJK")),
		layout.Rigid(spacer(12)),
		layout.Rigid(material.Body2(s.th, "Room password").Layout),
		layout.Rigid(s.editor(&s.joinPwEd, "The password the host gave you")),
		layout.Rigid(spacer(20)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "Join Room"
			if s.starting {
				label = "Joining…"
			}
			return material.Button(s.th, &s.joinRoomBtn, label).Layout(gtx)
		}),
		layout.Rigid(spacer(10)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.Button(s.th, &s.backBtn, "Back").Layout(gtx)
		}),
	}
	if s.status != "" {
		children = append(children, layout.Rigid(spacer(12)), layout.Rigid(material.Body2(s.th, s.status).Layout))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (s *state) layoutRoom(gtx layout.Context) layout.Dimensions {
	s.mu.Lock()
	live := s.live
	s.mu.Unlock()
	if live == nil {
		s.screen = screenFront
		return layout.Dimensions{}
	}
	if s.closeBtn.Clicked(gtx) {
		s.closeRoom()
		s.screen = screenFront
		s.status = ""
		return layout.Dimensions{}
	}
	members := live.nd.Members()

	children := []layout.FlexChild{}
	if live.hosting {
		children = append(children,
			layout.Rigid(material.Body2(s.th, "Your room code — read it out to your friends:").Layout),
			layout.Rigid(spacer(4)),
			layout.Rigid(material.H3(s.th, live.code).Layout),
			layout.Rigid(spacer(6)),
			layout.Rigid(material.Body2(s.th, "They'll also need the room password you just set.").Layout),
			layout.Rigid(spacer(18)),
		)
	} else {
		children = append(children,
			layout.Rigid(material.H5(s.th, "You're in!").Layout),
			layout.Rigid(spacer(12)),
		)
	}

	// Plain-language connection status while the room fills up.
	status := roomStatus(live, members)
	if status != "" {
		children = append(children,
			layout.Rigid(material.Body2(s.th, status).Layout),
			layout.Rigid(spacer(10)),
		)
	}
	if live.testMode {
		children = append(children,
			layout.Rigid(material.Caption(s.th, "Test mode on this computer: rooms work here, but games can't use this connection yet.").Layout),
			layout.Rigid(spacer(10)),
		)
	}

	children = append(children, layout.Rigid(material.H6(s.th, fmt.Sprintf("In this room (%d)", len(members))).Layout), layout.Rigid(spacer(6)))
	for _, m := range members {
		m := m
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.memberRow(gtx, m)
		}), layout.Rigid(spacer(4)))
	}
	children = append(children,
		layout.Rigid(spacer(16)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			label := "Leave Room"
			if live.hosting {
				label = "Close Room"
			}
			return material.Button(s.th, &s.closeBtn, label).Layout(gtx)
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// roomStatus turns core state into one honest sentence.
func roomStatus(live *liveRoom, members []node.MemberView) string {
	if len(members) > 1 {
		return ""
	}
	if live.hosting {
		return "Waiting for friends to join…"
	}
	if live.nd.HandshakeFailures > 0 {
		return "Wrong room password — leave and check it with the host."
	}
	if time.Since(live.joinedAt) > 30*time.Second {
		return "That code doesn't match a live room — check the code, or ask the host if the room is still open."
	}
	return "Looking for your room… this can take a few seconds."
}

func (s *state) memberRow(gtx layout.Context, m node.MemberView) layout.Dimensions {
	name := m.Name
	detail := ""
	switch {
	case m.Self:
		detail = "you"
	case !m.Direct:
		detail = "connected through " + m.Via
	default:
		detail = "connected directly"
	}
	if !m.Self && m.RTTMillis >= 0 {
		detail += fmt.Sprintf(" · %d ms", m.RTTMillis)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(material.Body1(s.th, name).Layout),
		layout.Rigid(material.Caption(s.th, detail).Layout),
	)
}

func (s *state) editor(ed *widget.Editor, hint string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return widget.Border{
			Color:        s.th.Fg,
			Width:        unit.Dp(1),
			CornerRadius: unit.Dp(4),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(8)).Layout(gtx, material.Editor(s.th, ed, hint).Layout)
		})
	}
}

func spacer(dp int) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Dimensions{Size: image.Pt(0, gtx.Dp(unit.Dp(dp)))}
	}
}
