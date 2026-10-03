package daemon

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fleetv1 "fleet/gen/fleetv1"
	"fleet/internal/adapter"
	"fleet/internal/config"
	"fleet/internal/identity"
	"fleet/internal/tmux"
)

const (
	stateWorking = fleetv1.AgentState_AGENT_STATE_WORKING
)

func TestUnixHandshakeRunAndExit(t *testing.T) {
	e := newEnv(t)
	c := e.dialUnix()
	if c.hello.GetAuthRequired() || c.hello.GetProtocolVersion() != fleetv1.ProtocolVersion_PROTOCOL_VERSION_1 ||
		len(c.hello.GetNonce()) != 32 || len(c.hello.GetServerId()) != 64 || c.hello.GetServerName() != "test" {
		t.Fatalf("bad hello: %v", c.hello)
	}
	e.mkdir("a")
	e.mkdir("b")

	exits := c.run(&fleetv1.RunAgentRequest{Root: "code", Path: "a", ExtraArgs: []string{"sleep 4; exit 3"}})
	fails := c.run(&fleetv1.RunAgentRequest{Root: "code", Path: "b", ExtraArgs: []string{"echo boom-$FLEET_AGENT_ID; exit 7"}})
	if exits.GetState() != stateRunning || exits.GetIsolation() != isoPinned || exits.GetCwd() != filepath.Join(e.root, "a") ||
		exits.GetSessionId() != "sess-"+exits.GetId() || exits.GetTmuxSession() != "fleet-"+exits.GetId() || len(exits.GetId()) != 6 {
		t.Fatalf("unexpected agent: %v", exits)
	}
	if !strings.HasPrefix(exits.GetName(), "test-a-") {
		t.Fatalf("generated name %q", exits.GetName())
	}
	live := c.ok(listReq(false)).GetListAgents().GetAgents()
	if len(live) == 0 || live[0].GetId() != exits.GetId() {
		t.Fatalf("ListAgents: %v", live)
	}

	f := c.waitAgent(fails.GetId(), "finished", finished)
	if f.GetState() != stateFailed || !f.GetHasExitCode() || f.GetExitCode() != 7 ||
		!strings.Contains(f.GetStateDetail(), "boom-"+fails.GetId()) {
		t.Fatalf("quick failure: %v", f)
	}
	x := c.waitAgent(exits.GetId(), "finished", finished)
	if x.GetState() != stateExited || !x.GetHasExitCode() || x.GetExitCode() != 3 {
		t.Fatalf("exit: %v", x)
	}
	if s := e.sessions(); len(s) != 0 {
		t.Fatalf("sessions left: %v", s)
	}
	if n := len(c.ok(listReq(false)).GetListAgents().GetAgents()); n != 0 {
		t.Fatalf("%d live agents after exit", n)
	}

	// agents.json is private and holds both agents.
	fi, err := os.Stat(config.Path("agents.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("agents.json: %v %v", fi, err)
	}
	// A second daemon on the same home refuses to start.
	err = Run(context.Background(), Options{Adapters: adapter.NewRegistry(), NoMDNS: true, Listen: "off", Web: "off"})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second daemon: %v", err)
	}
}

func TestRunValidation(t *testing.T) {
	e := newEnv(t)
	c := e.dialUnix()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.root, "escape")); err != nil {
		t.Fatal(err)
	}
	e.mkdir("plain")

	cases := []struct {
		name string
		req  *fleetv1.RunAgentRequest
		code fleetv1.ErrorCode
	}{
		{"absolute outside", &fleetv1.RunAgentRequest{AbsolutePath: outside}, codeOutside},
		{"symlink escape", &fleetv1.RunAgentRequest{Root: "code", Path: "escape"}, codeOutside},
		{"absolute symlink escape", &fleetv1.RunAgentRequest{AbsolutePath: filepath.Join(e.root, "escape")}, codeOutside},
		{"dotdot", &fleetv1.RunAgentRequest{Root: "code", Path: "../"}, codeOutside},
		{"missing dir", &fleetv1.RunAgentRequest{Root: "code", Path: "nope"}, codeNotFound},
		{"no path", &fleetv1.RunAgentRequest{}, codeInvalid},
		{"unknown adapter", &fleetv1.RunAgentRequest{Adapter: "nope", Root: "code"}, codeNotFound},
		{"unknown root", &fleetv1.RunAgentRequest{Root: "nope"}, codeNotFound},
		{"unavailable adapter", &fleetv1.RunAgentRequest{Adapter: "missing", Root: "code"}, codeUnavailable},
		{"worktree outside git", &fleetv1.RunAgentRequest{Root: "code", Path: "plain", Isolation: isoWorktree}, codeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.t = t
			c.fails(runReq(tc.req), tc.code)
		})
	}
	c.t = t
	if s := e.sessions(); len(s) != 0 {
		t.Fatalf("sessions: %v", s)
	}
}

func TestPinnedSharedAndKill(t *testing.T) {
	e := newEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Root: "code", Name: "api-fix"})
	// Several agents may be pinned to one folder.
	shared := c.run(&fleetv1.RunAgentRequest{AbsolutePath: e.root})
	if shared.GetIsolation() != isoPinned || a.GetIsolation() != isoPinned ||
		shared.GetCwd() != a.GetCwd() || shared.GetName() == a.GetName() {
		t.Fatalf("second pinned agent: %v, first: %v", shared, a)
	}
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: shared.GetId(), Forget: true}))
	e.mkdir("other")
	c.fails(runReq(&fleetv1.RunAgentRequest{Root: "code", Path: "other", Name: "api-fix"}), codeExists)

	// Hooks: routed to the adapter, but can never end an agent.
	hook := func(event, payload string) {
		c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Hook{Hook: &fleetv1.HookEvent{
			AgentId: a.GetId(), Adapter: "test", Event: event, Payload: []byte(payload),
		}}})
	}
	hook("working", "editing main.go")
	hook("exited", "")
	if got := c.agent(a.GetId()); got.GetState() != stateWorking || got.GetStateDetail() != "editing main.go" {
		t.Fatalf("after hook: %v", got)
	}

	c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_SendText{SendText: &fleetv1.SendTextRequest{Agent: "api-fix", Text: "x"}}})

	// Images are saved in the agent's state dir; anything else is refused.
	attach := func(data []byte) *fleetv1.ClientMessage {
		return &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_AttachImage{AttachImage: &fleetv1.AttachImageRequest{Agent: "api-fix", Data: data}}}
	}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	path := c.ok(attach(png)).GetAttachImage().GetPath()
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, png) ||
		filepath.Dir(path) != config.Path("agents", a.GetId(), imagesDir) || filepath.Ext(path) != ".png" {
		t.Fatalf("attached image at %q: %q, %v", path, got, err)
	}
	c.fails(attach([]byte("plain text")), codeInvalid)
	c.fails(attach(nil), codeInvalid)
	c.fails(attach(append(png, make([]byte, MaxImageSize)...)), codeInvalid)

	r := c.ok(killReq(&fleetv1.KillAgentRequest{Agent: "api-fix"})).GetKillAgent()
	if r.GetAgent().GetState() != stateExited || r.GetWorktreeKept() {
		t.Fatalf("kill: %v", r)
	}
	if s := e.sessions(); len(s) != 0 {
		t.Fatalf("sessions after kill: %v", s)
	}
	c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_SendText{SendText: &fleetv1.SendTextRequest{Agent: a.GetId(), Text: "x"}}}, codeInvalid)

	// The name is free again; the old agent stays in history until forgotten.
	b := c.run(&fleetv1.RunAgentRequest{Root: "code", Name: "api-fix"})
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: b.GetId(), Forget: true}))
	if c.agent(b.GetId()) != nil {
		t.Fatal("forgotten agent still listed")
	}
	if _, err := os.Stat(config.Path("agents", b.GetId())); !os.IsNotExist(err) {
		t.Fatalf("state dir kept: %v", err)
	}
	if c.agent(a.GetId()) == nil {
		t.Fatal("finished agent missing from history")
	}
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: a.GetId(), Forget: true}))
	c.fails(killReq(&fleetv1.KillAgentRequest{Agent: a.GetId()}), codeNotFound)
	if s := e.sessions(); len(s) != 0 {
		t.Fatalf("sessions: %v", s)
	}
}

func TestTLSPairingAndAuth(t *testing.T) {
	e := newEnv(t)
	local := e.dialUnix()
	dev, err := identity.LoadOrCreateDevice(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	c := e.dialTLS()
	if !c.hello.GetAuthRequired() || c.hello.GetServerId() != hex.EncodeToString(c.fp) {
		t.Fatalf("hello: %v (fp %x)", c.hello, c.fp)
	}
	c.fails(listReq(false), codeUnauth)
	c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Hook{Hook: &fleetv1.HookEvent{AgentId: "x"}}}, codeUnauth)
	if c.call(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Ping{Ping: &fleetv1.Ping{}}}).GetPong() == nil {
		t.Fatal("no pong")
	}

	code := local.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_CreatePairingCode{
		CreatePairingCode: &fleetv1.CreatePairingCodeRequest{},
	}}).GetCreatePairingCode()
	if code.GetServerId() != c.hello.GetServerId() || len(code.GetCode()) != 9 {
		t.Fatalf("code: %v", code)
	}
	pair := func(c *client, code string) *fleetv1.ClientMessage {
		return &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Pair{Pair: &fleetv1.PairRequest{
			DeviceName:      "phone",
			DevicePublicKey: dev.Public,
			Proof:           identity.PairProof(code, c.fp, dev.Public),
			DevicePlatform:  "test",
		}}}
	}
	c.fails(pair(c, "AAAA-AAAA"), codePairing)
	resp := c.ok(pair(c, code.GetCode())).GetPair()
	if resp.GetDeviceId() != dev.ID || resp.GetServerId() != c.hello.GetServerId() ||
		!bytes.Equal(resp.GetServerProof(), identity.ServerPairProof(code.GetCode(), c.fp, dev.Public)) {
		t.Fatalf("pair response: %v", resp)
	}
	c.ok(listReq(false)) // the pairing connection is authenticated
	c.fails(pair(c, code.GetCode()), codePairing)

	auth := func(c *client) *fleetv1.ClientMessage {
		return &fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Auth{Auth: &fleetv1.AuthRequest{
			DeviceId: dev.ID, Signature: dev.Sign(c.hello.GetNonce(), c.fp),
		}}}
	}
	c2 := e.dialTLS()
	if got := c2.ok(auth(c2)).GetAuth().GetDeviceName(); got != "phone" {
		t.Fatalf("auth device name %q", got)
	}
	c2.ok(listReq(false))

	devs := local.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_ListDevices{ListDevices: &fleetv1.ListDevicesRequest{}}}).GetListDevices().GetDevices()
	if len(devs) != 1 || devs[0].GetId() != dev.ID || !devs[0].GetConnected() || devs[0].GetPlatform() != "test" {
		t.Fatalf("devices: %v", devs)
	}

	// A signature over another connection's nonce is rejected.
	c3 := e.dialTLS()
	c3.fails(auth(c2), codeUnauth)

	// Revoking closes live connections and blocks future auth.
	local.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_RevokeDevice{RevokeDevice: &fleetv1.RevokeDeviceRequest{Device: "phone"}}})
	if !c2.closed(5*time.Second) || !c.closed(5*time.Second) {
		t.Fatal("revoked device's connections stay open")
	}
	c4 := e.dialTLS()
	c4.fails(auth(c4), codeUnauth)
	if !c4.closed(5 * time.Second) {
		t.Fatal("connection open after auth by revoked device")
	}

	// Three failed attempts close the connection.
	c5 := e.dialTLS()
	c5.fails(pair(c5, "BBBB-BBBB"), codePairing)
	c5.fails(pair(c5, "CCCC-CCCC"), codePairing)
	c5.fails(pair(c5, "DDDD-DDDD"), codePairing)
	if !c5.closed(5 * time.Second) {
		t.Fatal("connection open after 3 failed pairing attempts")
	}
}

func TestAttach(t *testing.T) {
	e := newEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Root: "code", ExtraArgs: []string{"echo hello; sleep 5"}, Cols: 80, Rows: 24})

	term := e.dialUnix()
	if got := term.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Attach{Attach: &fleetv1.AttachRequest{Agent: a.GetName(), Cols: 80, Rows: 24}}}).GetAttach(); got.GetAgentId() != a.GetId() {
		t.Fatalf("attach: %v", got)
	}
	term.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Attach{Attach: &fleetv1.AttachRequest{Agent: a.GetId()}}}, codeExists)
	var out bytes.Buffer
	deadline := time.Now().Add(10 * time.Second)
	for !bytes.Contains(out.Bytes(), []byte("hello")) {
		if time.Now().After(deadline) {
			t.Fatalf("no hello in terminal output: %q", out.String())
		}
		if m := term.next(time.Second); m.GetTerminalOutput() != nil {
			if m.GetTerminalOutput().GetAgentId() != a.GetId() {
				t.Fatalf("output for %q", m.GetTerminalOutput().GetAgentId())
			}
			out.Write(m.GetTerminalOutput().GetData())
		}
	}
	term.push(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_TerminalResize{TerminalResize: &fleetv1.TerminalResize{AgentId: a.GetId(), Cols: 100, Rows: 30}}})
	c.waitAgent(a.GetId(), "attached", func(a *fleetv1.Agent) bool { return a.GetAttachedClients() == 1 })

	term.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Detach{Detach: &fleetv1.DetachRequest{AgentId: a.GetId()}}})
	if r := closedReason(t, term); r != "detached" {
		t.Fatalf("closed reason %q", r)
	}
	if got := c.agent(a.GetId()); !isLive(got.GetState()) {
		t.Fatalf("detach ended the agent: %v", got)
	}

	// Read-only attach rejects input; the terminal closes when the agent ends.
	ro := e.dialUnix()
	ro.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Attach{Attach: &fleetv1.AttachRequest{Agent: a.GetId(), ReadOnly: true}}})
	// Fire-and-forget (id 0) input is dropped without an Error reply; a
	// request with an id gets one.
	ro.push(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_TerminalInput{TerminalInput: &fleetv1.TerminalInput{AgentId: a.GetId(), Data: []byte("x")}}})
	ro.push(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_TerminalInput{TerminalInput: &fleetv1.TerminalInput{AgentId: "nope", Data: []byte("x")}}})
	ro.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Ping{Ping: &fleetv1.Ping{}}})
	for _, m := range ro.backlog {
		if m.GetError() != nil {
			t.Fatalf("error reply to an id-0 message: %v", m)
		}
	}
	ro.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_TerminalInput{TerminalInput: &fleetv1.TerminalInput{AgentId: a.GetId(), Data: []byte("x")}}}, codeDenied)
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: a.GetId()}))
	if r := closedReason(t, ro); r != "terminal ended" {
		t.Fatalf("closed reason %q", r)
	}
	if s := e.sessions(); len(s) != 0 {
		t.Fatalf("sessions: %v", s)
	}
}

// closedReason skips output until TerminalClosed and returns its reason.
func closedReason(t *testing.T, c *client) string {
	t.Helper()
	for {
		m := c.next(10 * time.Second)
		if m == nil {
			t.Fatal("no TerminalClosed")
		}
		if tc := m.GetTerminalClosed(); tc != nil {
			return tc.GetReason()
		}
	}
}

func TestSubscribe(t *testing.T) {
	e := newEnv(t)
	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Root: "code"})

	sub := e.dialUnix()
	sub.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Subscribe{Subscribe: &fleetv1.SubscribeRequest{}}})
	next := func() *fleetv1.Event {
		t.Helper()
		m := sub.next(10 * time.Second)
		if m.GetEvent() == nil || m.GetId() != 0 {
			t.Fatalf("expected event, got %v", m)
		}
		return m.GetEvent()
	}
	if ev := next(); ev.GetAgentUpserted().GetId() != a.GetId() {
		t.Fatalf("snapshot: %v", ev)
	}
	if ev := next(); ev.GetSnapshotDone() == nil {
		t.Fatalf("want SnapshotDone, got %v", ev)
	}

	e.mkdir("b")
	b := c.run(&fleetv1.RunAgentRequest{Root: "code", Path: "b"})
	for {
		ev := next()
		if ev.GetAgentUpserted().GetId() == b.GetId() && ev.GetAgentUpserted().GetState() == stateRunning {
			break
		}
	}
	dir := t.TempDir()
	root := c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_AddRoot{AddRoot: &fleetv1.AddRootRequest{Path: dir}}}).GetAddRoot().GetRoot()
	for {
		if rc := next().GetRootsChanged(); rc != nil {
			if len(rc.GetRoots()) != 2 || rc.GetRoots()[1].GetName() != root.GetName() {
				t.Fatalf("roots changed: %v", rc)
			}
			break
		}
	}
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: b.GetId(), Forget: true}))
	for next().GetAgentRemoved() != b.GetId() {
	}

	// Subscribing again on the same connection sends a fresh snapshot.
	sub.backlog = nil
	for len(sub.msgs) > 0 {
		<-sub.msgs
	}
	sub.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Subscribe{Subscribe: &fleetv1.SubscribeRequest{}}})
	sawA := false
	for {
		ev := next()
		if ev.GetAgentUpserted().GetId() == a.GetId() {
			sawA = true
		}
		if ev.GetSnapshotDone() != nil {
			break
		}
	}
	if !sawA {
		t.Fatal("repeated subscribe: snapshot lacks the running agent")
	}
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: a.GetId()}))
}

func TestRestartReadoptsAndKillsOrphans(t *testing.T) {
	e := newEnv(t)
	c := e.dialUnix()
	keep := c.run(&fleetv1.RunAgentRequest{Root: "code"})
	e.mkdir("b")
	gone := c.run(&fleetv1.RunAgentRequest{Root: "code", Path: "b"})
	e.stop()

	ctx := context.Background()
	if err := e.tm.KillSession(ctx, gone.GetTmuxSession()); err != nil {
		t.Fatal(err)
	}
	if err := e.tm.NewSession(ctx, tmux.NewSessionOptions{Name: "fleet-orphan", Argv: []string{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	e.start()
	c = e.dialUnix()
	if got := c.agent(keep.GetId()); got.GetState() != stateRunning {
		t.Fatalf("kept agent: %v", got)
	}
	if got := c.agent(gone.GetId()); got.GetState() != stateExited {
		t.Fatalf("vanished agent: %v", got)
	}
	if s := e.sessions(); len(s) != 1 || s[0] != keep.GetTmuxSession() {
		t.Fatalf("sessions after restart: %v", s)
	}
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: keep.GetId()}))
	if s := e.sessions(); len(s) != 0 {
		t.Fatalf("sessions: %v", s)
	}
}

func TestWorktreeAndBrowse(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	e := newEnv(t)
	repo := e.mkdir("repo")
	e.mkdir("repo/sub")
	e.mkdir(".hidden")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "sub", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "init")

	c := e.dialUnix()
	a := c.run(&fleetv1.RunAgentRequest{Root: "code", Path: "repo/sub", Name: "Fix Bug"})
	if a.GetIsolation() != isoWorktree || a.GetBranch() != "fleet/fix-bug" ||
		!strings.HasPrefix(a.GetCwd(), config.Path("worktrees")) || filepath.Base(a.GetCwd()) != "sub" {
		t.Fatalf("worktree agent: %v", a)
	}
	// Worktree agents do not pin the directory.
	pinned := c.run(&fleetv1.RunAgentRequest{Root: "code", Path: "repo/sub", Isolation: isoPinned})

	br := c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Browse{Browse: &fleetv1.BrowseRequest{Root: "code"}}}).GetBrowse()
	if len(br.GetEntries()) != 1 || br.GetEntries()[0].GetName() != "repo" || !br.GetEntries()[0].GetIsGitRepo() ||
		br.GetEntries()[0].GetAgentCount() != 2 || br.GetPath() != "" {
		t.Fatalf("browse: %v", br)
	}
	br = c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Browse{Browse: &fleetv1.BrowseRequest{Root: "code", IncludeHidden: true}}}).GetBrowse()
	if len(br.GetEntries()) != 2 || br.GetEntries()[0].GetName() != ".hidden" {
		t.Fatalf("browse hidden: %v", br)
	}
	c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Browse{Browse: &fleetv1.BrowseRequest{Root: "code", Path: ".."}}}, codeOutside)

	wt := filepath.Dir(a.GetCwd())
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := c.ok(killReq(&fleetv1.KillAgentRequest{Agent: a.GetId(), RemoveWorktree: true})).GetKillAgent()
	if !r.GetWorktreeKept() || r.GetWorktreeKeptReason() == "" {
		t.Fatalf("dirty worktree not kept: %v", r)
	}
	r = c.ok(killReq(&fleetv1.KillAgentRequest{Agent: a.GetId(), RemoveWorktree: true, Force: true, Forget: true})).GetKillAgent()
	if r.GetWorktreeKept() {
		t.Fatalf("forced removal kept worktree: %v", r)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	c.ok(killReq(&fleetv1.KillAgentRequest{Agent: pinned.GetId()}))
	if s := e.sessions(); len(s) != 0 {
		t.Fatalf("sessions: %v", s)
	}
}

func TestInfoAdaptersRoots(t *testing.T) {
	e := newEnv(t)
	c := e.dialUnix()
	info := c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_GetInfo{GetInfo: &fleetv1.GetInfoRequest{}}}).GetGetInfo()
	if info.GetServerId() != c.hello.GetServerId() || info.GetDaemonVersion() != "test" || info.GetOs() == "" || info.GetStartedAtMs() == 0 {
		t.Fatalf("info: %v", info)
	}
	ads := c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_ListAdapters{ListAdapters: &fleetv1.ListAdaptersRequest{}}}).GetListAdapters().GetAdapters()
	if len(ads) != 3 || ads[0].GetId() != "missing" || ads[0].GetAvailable() || ads[0].GetUnavailableReason() == "" ||
		ads[1].GetId() != "modeled" || len(ads[1].GetModels()) != 2 || len(ads[1].GetEfforts()) != 2 ||
		ads[2].GetId() != "test" || !ads[2].GetAvailable() || !ads[2].GetCapabilities().GetActivityState() || len(ads[2].GetModels()) != 0 {
		t.Fatalf("adapters: %v", ads)
	}

	// A root restricted to another adapter refuses "test".
	dir := t.TempDir()
	r := c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_AddRoot{AddRoot: &fleetv1.AddRootRequest{
		Path: dir, Name: "only-missing", Adapters: []string{"missing"},
	}}}).GetAddRoot().GetRoot()
	if r.GetName() != "only-missing" {
		t.Fatalf("root: %v", r)
	}
	c.fails(runReq(&fleetv1.RunAgentRequest{Root: "only-missing"}), codeDenied)
	c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_AddRoot{AddRoot: &fleetv1.AddRootRequest{Path: dir}}}, codeInvalid)
	c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_AddRoot{AddRoot: &fleetv1.AddRootRequest{Path: "rel"}}}, codeInvalid)

	cfg, err := config.Load()
	if err != nil || len(cfg.Roots) != 2 {
		t.Fatalf("saved config: %v %v", cfg, err)
	}
	c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_RemoveRoot{RemoveRoot: &fleetv1.RemoveRootRequest{Name: "only-missing"}}})
	c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_RemoveRoot{RemoveRoot: &fleetv1.RemoveRootRequest{Name: "only-missing"}}}, codeNotFound)
	roots := c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_ListRoots{ListRoots: &fleetv1.ListRootsRequest{}}}).GetListRoots().GetRoots()
	if len(roots) != 1 || roots[0].GetName() != "code" || roots[0].GetPath() != e.root {
		t.Fatalf("roots: %v", roots)
	}
	c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Browse{Browse: &fleetv1.BrowseRequest{Root: "only-missing"}}}, codeNotFound)
	c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_AddRoot{AddRoot: &fleetv1.AddRootRequest{Path: dir, Adapters: []string{"nope"}}}}, codeNotFound)
	// Local connections cannot pair: wrong transport.
	c.fails(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Pair{Pair: &fleetv1.PairRequest{}}}, codeUnauth)
}

// TestStartupRoots checks Options.Roots (`fleet daemon --root`): new folders
// become roots and are saved, existing roots are kept as they are, and a
// missing folder stops the daemon from starting.
func TestStartupRoots(t *testing.T) {
	e := newEnv(t)
	e.stop()
	here, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.roots = []config.Root{{Path: here, Trust: true}, {Path: e.root, Trust: true}}
	e.start()
	c := e.dialUnix()
	list := func() []*fleetv1.Root {
		return c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_ListRoots{ListRoots: &fleetv1.ListRootsRequest{}}}).GetListRoots().GetRoots()
	}
	roots := list()
	if len(roots) != 2 || roots[0].GetName() != "code" || roots[0].GetTrust() ||
		roots[1].GetPath() != here || roots[1].GetName() != filepath.Base(here) || !roots[1].GetTrust() {
		t.Fatalf("roots: %v", roots)
	}

	// Starting again with the same roots adds nothing.
	e.stop()
	e.start()
	c = e.dialUnix()
	if roots := list(); len(roots) != 2 {
		t.Fatalf("roots after restart: %v", roots)
	}
	if cfg, err := config.Load(); err != nil || len(cfg.Roots) != 2 {
		t.Fatalf("saved config: %+v, %v", cfg, err)
	}

	e.stop()
	opts := Options{Adapters: adapter.NewRegistry(testAdapter{}), Web: "off", NoMDNS: true, Listen: "off",
		Roots: []config.Root{{Path: filepath.Join(here, "missing")}}}
	if err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Run with a missing root: %v", err)
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	m := &manager{d: &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}, subs: map[*subscriber]struct{}{}}
	s := &subscriber{ch: make(chan *fleetv1.Event, 1)}
	m.subs[s] = struct{}{}
	ev := &fleetv1.Event{Kind: &fleetv1.Event_AgentRemoved{AgentRemoved: "x"}}
	m.broadcastLocked(ev)
	m.broadcastLocked(ev) // buffer full: dropped, not blocked
	if len(m.subs) != 0 {
		t.Fatal("slow subscriber still registered")
	}
	if <-s.ch != ev {
		t.Fatal("queued event lost")
	}
	if _, ok := <-s.ch; ok {
		t.Fatal("channel not closed")
	}
	m.unsubscribe(s) // no double close
}

func TestScreenPromptAndTrustDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	e := newEnv(t)
	c := e.dialUnix()
	out := t.TempDir()

	// A trusted root holding a repository with a linked worktree.
	trusted, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(trusted, "repo")
	for _, d := range []string{filepath.Join(repo, "sub"), filepath.Join(trusted, "loose")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "init")
	git("worktree", "add", "-q", "-b", "side", filepath.Join(trusted, "side"))
	root := c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_AddRoot{AddRoot: &fleetv1.AddRootRequest{
		Path: trusted, Name: "trusted", Trust: true,
	}}}).GetAddRoot().GetRoot()
	if !root.GetTrust() {
		t.Fatalf("added root: %v", root)
	}

	// The adapter gets the main checkout as TrustDir, from the repo, a
	// subdirectory, a user's worktree and a plain folder; nothing in an
	// untrusted root.
	e.mkdir("plain")
	for i, tc := range []struct{ root, path, want string }{
		{"trusted", "repo/sub", repo},
		{"trusted", "side", repo},
		{"trusted", "loose", filepath.Join(trusted, "loose")},
		{"code", "plain", ""},
	} {
		file := filepath.Join(out, fmt.Sprint(i))
		a := c.run(&fleetv1.RunAgentRequest{Root: tc.root, Path: tc.path, Isolation: isoPinned,
			ExtraArgs: []string{fmt.Sprintf(`printf %%s "$TEST_TRUST_DIR" > %q; sleep 60`, file)}})
		waitFor(t, "trust dir file", func() bool { _, err := os.Stat(file); return err == nil })
		if got, _ := os.ReadFile(file); string(got) != tc.want {
			t.Errorf("%s:%s: TrustDir = %q, want %q", tc.root, tc.path, got, tc.want)
		}
		c.ok(killReq(&fleetv1.KillAgentRequest{Agent: a.GetId(), Forget: true}))
	}

	// A dialog on the screen makes a starting agent NEEDS_INPUT until it goes.
	a := c.run(&fleetv1.RunAgentRequest{Root: "code", Path: "plain", Isolation: isoPinned,
		ExtraArgs: []string{"printf '" + testDialog + "'; read x; clear; read x; printf '" + testDialog + "'; sleep 60"}})
	c.waitAgent(a.GetId(), "asking", func(a *fleetv1.Agent) bool {
		return a.GetState() == stateNeedsInput && a.GetStateDetail() == "test dialog"
	})
	c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_SendText{SendText: &fleetv1.SendTextRequest{Agent: a.GetId(), Text: "y", Submit: true}}})
	c.waitAgent(a.GetId(), "running", func(a *fleetv1.Agent) bool {
		return a.GetState() == stateRunning && a.GetStateDetail() == ""
	})

	// Once a hook has reported state, the screen is not watched any more.
	c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_Hook{Hook: &fleetv1.HookEvent{
		AgentId: a.GetId(), Adapter: "test", Event: "working", Payload: []byte("busy"),
	}}})
	c.ok(&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_SendText{SendText: &fleetv1.SendTextRequest{Agent: a.GetId(), Text: "y", Submit: true}}})
	time.Sleep(3 * reconcileInterval)
	if got := c.agent(a.GetId()); got.GetState() != stateWorking {
		t.Fatalf("after a hook the screen changed the state: %v", got)
	}
}
