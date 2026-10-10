# Fleet wire protocol v1: guide for native clients

This is the complete guide for writing a fleet client: the macOS app first,
later iOS, Android, Windows or anything else. It covers everything that is
not obvious from the message definitions in
[`proto/fleet/v1/fleet.proto`](../proto/fleet/v1/fleet.proto): discovery,
transport, TLS pinning, framing, pairing, authentication, the semantics and
error codes of every request, events, terminal streaming and the agent
state machine.

The Go client in `internal/client` is the reference implementation. All
statements here were checked against the daemon code in `internal/daemon`,
`internal/identity`, `internal/wire` and `internal/discovery`. The test
vectors at the end are asserted by `internal/identity/vectors_test.go` and
`internal/wire/vectors_test.go`, so the document and the code cannot drift
apart silently.

Contents:

1. [Overview](#1-overview)
2. [Discovery (mDNS / DNS-SD)](#2-discovery-mdns--dns-sd)
3. [Transport and TLS pinning](#3-transport-and-tls-pinning)
4. [Framing](#4-framing)
5. [Code generation](#5-code-generation)
6. [Handshake](#6-handshake)
7. [Pairing](#7-pairing)
8. [Authentication](#8-authentication)
9. [Storing the device key and paired servers](#9-storing-the-device-key-and-paired-servers)
10. [Requests, responses and pushed messages](#10-requests-responses-and-pushed-messages)
11. [RPC reference](#11-rpc-reference)
12. [Events (Subscribe)](#12-events-subscribe)
13. [Terminal streaming](#13-terminal-streaming)
14. [Agent state machine](#14-agent-state-machine)
15. [Connection rules and limits](#15-connection-rules-and-limits)
16. [Worked examples](#16-worked-examples)
17. [Test vectors](#17-test-vectors)
18. [Client checklist](#18-client-checklist)

---

## 1. Overview

```
 macOS app                                   server (daemon host)
 ─────────                                   ─────────────────────
 NWBrowser  ── mDNS _fleet._tcp ──────────▶  fleet daemon (advertises)
 NWConnection ─ TCP :7420 + TLS 1.3 ──────▶  self-signed cert, pinned by SHA-256
      │   4-byte length + protobuf frames
      │   ServerHello ◀──
      │   PairRequest / AuthRequest ──▶
      │   requests ──▶ ◀── responses, events, terminal output
      ▼
 SwiftTerm  ◀── raw VT bytes ── tmux client in a PTY ── tmux session "fleet-<id>"
```

- One daemon per server. It runs agent CLIs (Claude Code, Codex, a shell)
  inside tmux sessions and keeps them running across daemon restarts.
- Clients pair once with a short code, then authenticate with an Ed25519
  device key on every connection. There are no accounts and no cloud relay.
- Every paired device has full access in v1: it can do everything the
  local CLI can do, including pairing and revoking other devices.

## 2. Discovery (mDNS / DNS-SD)

The daemon advertises itself when its TCP listener is on and mDNS is enabled
(`mdns = true`, the default, and no `--no-mdns` flag).

| Item          | Value |
|---------------|-------|
| Service type  | `_fleet._tcp` |
| Domain        | `local.` |
| Instance name | the server name (`name` in `config.toml`, default the hostname), cut to 63 bytes |
| SRV port      | the TLS port (default 7420) |
| TXT `v`       | `1`, the protocol version. Ignore services with any other value. |
| TXT `id`      | the server id: full lowercase hex SHA-256 of the daemon's certificate (64 hex digits) |
| TXT `name`    | the server name (at most 200 bytes) |
| TXT `web`     | optional: the port of the daemon's web dashboard (plain HTTP, same host), present when it is reachable from the LAN. Apps may offer to open `http://<address>:<web>/`. |

How to use it:

- Show discovered daemons by `name`. Use `id` to recognise a daemon you have
  already paired with, even if its IP address changed.
- mDNS is not authenticated. Anyone on the LAN can advertise any `id`. The
  TLS pin (section 3) is what actually identifies a daemon; the TXT `id` is
  only a hint for finding it.
- The default listener is IPv4 only (`0.0.0.0:7420`). Prefer the IPv4
  addresses of a resolved service. The Go client drops IPv6 link-local
  addresses because mDNS answers carry no zone.
- For a paired server, first try the last address that worked. If that
  fails, browse for a service whose TXT `id` equals the stored server id and
  try its addresses. Update the stored address when one works. This is what
  `client.Connect` does.

On Apple platforms:

```swift
import Network

let browser = NWBrowser(for: .bonjourWithTXTRecord(type: "_fleet._tcp", domain: nil),
                        using: .tcp)
browser.browseResultsChangedHandler = { results, _ in
    for r in results {
        guard case let .service(name, _, _, _) = r.endpoint,
              case let .bonjour(txt) = r.metadata,
              txt["v"] == "1", let id = txt["id"] else { continue }
        let display = txt["name"] ?? name
        // r.endpoint can be passed straight to NWConnection (section 3).
        print(display, id)
    }
}
browser.start(queue: .main)
```

The app needs `NSLocalNetworkUsageDescription` and `NSBonjourServices =
["_fleet._tcp"]` in its Info.plist. A sandboxed macOS app also needs the
`com.apple.security.network.client` entitlement.

## 3. Transport and TLS pinning

| Transport | Address | Authentication |
|-----------|---------|----------------|
| Remote    | TCP, default `0.0.0.0:7420` (`listen` in `config.toml`; `"off"` disables it) | TLS 1.3, then pairing or device auth |
| Local     | Unix socket `$FLEET_HOME/fleet.sock` (mode 0600; `FLEET_HOME` defaults to `~/.fleet`) | none: `ServerHello.auth_required = false` |

Native apps use the remote transport. The local socket is for the CLI and
for `fleet hook` on the daemon host.

TLS rules:

- TLS 1.3 only. The daemon has one long-lived self-signed ECDSA P-256
  certificate (valid 10 years). It never asks for a client certificate;
  devices authenticate inside the protocol instead.
- Do **not** validate the chain or the hostname. Instead compute
  `fp = SHA-256(DER of the leaf certificate)`, 32 bytes. `hex(fp)` in lower
  case is the **server id**.
- Paired server: refuse the TLS handshake unless `hex(fp)` equals the stored
  server id.
- Pairing (no server id yet): accept any certificate, but remember `fp`.
  The user must confirm it before the pairing proof is sent (section 7).
- Always keep the `fp` observed on *this* connection. The pairing proof and
  the auth signature are bound to it, so a man in the middle who presents
  another certificate cannot reuse them.
- After `ServerHello` arrives, check that `hello.server_id == hex(fp)` and
  close the connection if it does not.
- The daemon closes a connection whose TLS handshake takes longer than 15 s.

Network.framework sketch (macOS 12+ / iOS 15+):

```swift
import Network
import CryptoKit
import Security

extension Data {
    var hex: String { map { String(format: "%02x", $0) }.joined() }
}

/// Holds the certificate fingerprint observed during the TLS handshake.
final class ObservedFingerprint: @unchecked Sendable {
    private let lock = NSLock()
    private var _value: Data?
    var value: Data? { lock.withLock { _value } }
    func set(_ v: Data) { lock.withLock { _value = v } }
}

/// pin: the stored server id (64 lowercase hex digits), or nil while pairing.
func fleetParameters(pin: String?, observed: ObservedFingerprint) -> NWParameters {
    let tls = NWProtocolTLS.Options()
    let sec = tls.securityProtocolOptions
    sec_protocol_options_set_min_tls_protocol_version(sec, .TLSv13)
    sec_protocol_options_set_verify_block(sec, { _, secTrust, complete in
        let trust = sec_trust_copy_ref(secTrust).takeRetainedValue()
        guard let chain = SecTrustCopyCertificateChain(trust) as? [SecCertificate],
              let leaf = chain.first else { complete(false); return }
        let der = SecCertificateCopyData(leaf) as Data
        let fp = Data(SHA256.hash(data: der))
        observed.set(fp)
        if let pin {
            complete(fp.hex == pin)   // paired: only the pinned certificate
        } else {
            complete(true)            // pairing: user confirms fp.hex before the proof is sent
        }
    }, DispatchQueue(label: "fleet.tls.verify"))

    let tcp = NWProtocolTCP.Options()
    tcp.enableKeepalive = true
    tcp.keepaliveIdle = 30
    return NWParameters(tls: tls, tcp: tcp)
}

// Connect to a stored address or straight to a Bonjour result:
// let conn = NWConnection(host: "192.168.1.20", port: 7420, using: fleetParameters(pin: id, observed: fp))
// let conn = NWConnection(to: browseResult.endpoint, using: fleetParameters(pin: nil, observed: fp))
```

## 4. Framing

Each message on the stream is one frame:

```
+--------------------------+-------------------------------+
| length: uint32 big-endian | protobuf bytes (length bytes) |
+--------------------------+-------------------------------+
```

- Client to daemon: `fleet.v1.ClientMessage`. Daemon to client:
  `fleet.v1.ServerMessage`. There are no other message types on the wire.
- The maximum frame is 4 MiB (4194304 bytes) in both directions.
- Until a TLS connection has paired or authenticated, the daemon accepts at
  most 64 KiB per frame. A larger length prefix closes the connection.
- A frame that is not valid protobuf closes the connection.
- A length of 0 is a valid, empty message.
- Frames are never interleaved: every write is one whole frame. Serialize
  your writes (for example with an actor or a serial queue).

Swift sketch:

```swift
func sendFrame(_ conn: NWConnection, _ msg: Fleet_V1_ClientMessage) throws {
    let body: Data = try msg.serializedBytes()   // serializedData() on older SwiftProtobuf
    var len = UInt32(body.count).bigEndian
    var frame = Data(bytes: &len, count: 4)
    frame.append(body)
    conn.send(content: frame, completion: .contentProcessed { _ in })
}

func receiveFrame(_ conn: NWConnection,
                  _ done: @escaping (Result<Fleet_V1_ServerMessage, Error>) -> Void) {
    conn.receive(minimumIncompleteLength: 4, maximumLength: 4) { hdr, _, _, err in
        if let err { return done(.failure(err)) }
        guard let hdr, hdr.count == 4 else { return done(.failure(FleetError.closed)) }
        let n = hdr.reduce(0) { $0 << 8 | Int($1) }
        guard n <= 4 << 20 else { return done(.failure(FleetError.frameTooLarge)) }
        if n == 0 { return done(Result { try Fleet_V1_ServerMessage(serializedBytes: Data()) }) }
        conn.receive(minimumIncompleteLength: n, maximumLength: n) { body, _, _, err in
            if let err { return done(.failure(err)) }
            guard let body, body.count == n else { return done(.failure(FleetError.closed)) }
            done(Result { try Fleet_V1_ServerMessage(serializedBytes: body) })
        }
    }
}
```

## 5. Code generation

Generate message types from `proto/fleet/v1/fleet.proto` with
[buf](https://buf.build). The Go code in `gen/fleetv1` comes from
`buf.gen.yaml` (`make proto`). For Swift, save this as
`buf.gen.swift.yaml` next to `buf.yaml` in the fleet repository:

```yaml
# buf.gen.swift.yaml: Swift types for the fleet protocol.
# Run from the fleet repository root:
#   buf generate --template buf.gen.swift.yaml
version: v2
plugins:
  # Remote plugin, runs on the Buf Schema Registry (no local install needed).
  # Pin a version to keep builds reproducible.
  - remote: buf.build/apple/swift
    out: ../fleet-macos/Sources/FleetProto
    opt:
      - Visibility=Public
      - FileNaming=DropPath
  # Or with a locally installed protoc-gen-swift (brew install swift-protobuf):
  # - local: protoc-gen-swift
  #   out: ../fleet-macos/Sources/FleetProto
  #   opt: [Visibility=Public, FileNaming=DropPath]
inputs:
  - directory: proto
```

Adjust `out` to your app project. The output is one file, `fleet.pb.swift`.
Add the [swift-protobuf](https://github.com/apple/swift-protobuf) runtime
package to the app, same major version as the generator. The generated
types are prefixed with the proto package: `Fleet_V1_ClientMessage`,
`Fleet_V1_ServerMessage`, `Fleet_V1_Agent`, and so on. A `oneof` becomes an
optional enum (`msg: Fleet_V1_ClientMessage.OneOf_Msg?`) with one accessor
per case (`m.ping = Fleet_V1_Ping()` selects that case).

No RPC stubs are needed or wanted: the protocol is not gRPC. You only need
the message types.

Other platforms, same `inputs` block:

| Platform | Plugin(s) | Notes |
|----------|-----------|-------|
| Kotlin / Android | `remote: buf.build/protocolbuffers/java` plus `remote: buf.build/protocolbuffers/kotlin` (both with `opt: lite` for Android) | Kotlin DSL builders on top of the Java classes; runtime `com.google.protobuf:protobuf-kotlin-lite` |
| C# / .NET | `remote: buf.build/protocolbuffers/csharp` | runtime NuGet `Google.Protobuf` |
| TypeScript | `remote: buf.build/bufbuild/es` | runtime `@bufbuild/protobuf` |

Unknown fields and unknown enum values must be tolerated: newer daemons may
add fields within protocol version 1.

## 6. Handshake

The daemon speaks first. On every connection, local or remote:

1. The daemon sends `ServerMessage{id: 0, hello: ServerHello}` without being
   asked:
   - `protocol_version`: `PROTOCOL_VERSION_1` (1). Refuse any other value.
   - `server_id`: hex SHA-256 of the certificate. Must equal `hex(fp)`.
   - `server_name`, `daemon_version`.
   - `nonce`: 32 random bytes, new for every connection. Sign it in
     `AuthRequest`.
   - `auth_required`: `true` on TLS, `false` on the local Unix socket.
2. If `auth_required` is true, the only requests accepted before pairing or
   authenticating are `PairRequest`, `AuthRequest` and `Ping`. Anything else
   gets `Error{ERROR_CODE_UNAUTHENTICATED, "authenticate or pair first"}`.
3. The client sends `PairRequest` (first time) or `AuthRequest` (already
   paired). On success the connection is authenticated for its lifetime.

A client never has to send a version. The daemon checks nothing about the
client except the pairing proof or signature.

## 7. Pairing

Pairing registers a new device's Ed25519 public key with the daemon, using
a short one-time code shown on the server.

### 7.1 What the user sees

On the server (or on any already paired device), `fleet pair` prints:

```
Pairing code for studio (single use, expires 14:05:09, in 5m):

        A B C D - E F G H

Server fingerprint: 3b44 05b1 be5c 9a1f 7d20 c4e6 a8b1 f03e 5d7c 9b2a 4e6f 8a0c 1d3e 5f7a 9b0c 2d4e
(the new device must confirm this fingerprint before it sends the code)

On the other machine run:

    fleet connect studio --code ABCD-EFGH --fingerprint 3b4405b1be5c9a1f
```

A paired app can issue a code itself with `CreatePairingCodeRequest` and
show the code plus `server_id` the same way.

### 7.2 The code

- 8 characters of Crockford base32, alphabet
  `0123456789ABCDEFGHJKMNPQRSTVWXYZ`, so 40 bits. Displayed as `ABCD-EFGH`.
- Single use. It expires after its TTL: 300 s by default, and the daemon
  never allows more than 300 s.
- Issuing a new code invalidates the previous one. At most one code is live.
- Each failed `PairRequest` counts against the live code; after 5 failures
  the code is gone and the user must create a new one.

**Normalization.** Both sides compute the proof from the normalized code:

1. Convert to upper case.
2. Remove `-`, space and tab.
3. Map `O` to `0`, and `I` and `L` to `1`.
4. The result must be exactly 8 characters, all from the alphabet above.

If the user's input does not normalize, reject it locally and do not send
anything: every failed attempt uses up one of the code's 5 tries.

### 7.3 Sequence

```
Client                                                   Daemon
  |                                                        |
  |---- TCP connect, TLS 1.3 handshake -------------------->|
  |     (no pin; record fp = SHA-256(leaf cert DER))       |
  |<--- ServerMessage{id:0, hello}  -----------------------|
  |     check protocol_version == 1                        |
  |     check hello.server_id == hex(fp)                   |
  |                                                        |
  |  ** show hex(fp) to the user; they confirm it matches  |
  |  ** the fingerprint `fleet pair` printed (at least the |
  |  ** first 16 hex digits). Abort if not.                |
  |                                                        |
  |---- ClientMessage{id:1, pair: PairRequest{            |
  |        device_name, device_public_key (32 B),         |
  |        proof, device_platform}} ---------------------->|
  |                                   redeem code: HMAC   |
  |                                   check, store device |
  |<--- ServerMessage{id:1, pair: PairResponse{           |
  |        device_id, server_proof, server_id,            |
  |        server_name}} ----------------------------------|
  |     verify server_proof (constant time)                |
  |     store {server_id = hex(fp), device_id, name, addr} |
  |                                                        |
  |     connection is authenticated; send requests         |
```

On failure the daemon replies `Error{ERROR_CODE_PAIRING_FAILED}` (wrong,
expired or used-up code) or `Error{ERROR_CODE_INVALID_ARGUMENT}` (public key
not 32 bytes). See section 15 for when it also closes the connection.

### 7.4 Byte constructions

All `||` are plain byte concatenation, no length prefixes or separators.

```
key          = UTF-8(normalized code)                        # 8 bytes, e.g. "ABCDEFGH"
fp           = SHA-256(server certificate DER)               # 32 bytes, observed on this connection
pub          = device Ed25519 public key                     # 32 bytes

proof        = HMAC-SHA256(key, "fleet-pair-v1"        || fp || pub)   # 32 bytes, sent in PairRequest.proof
server_proof = HMAC-SHA256(key, "fleet-pair-server-v1" || fp || pub)   # 32 bytes, received in PairResponse
device_id    = lowercase hex of the first 16 bytes of SHA-256(pub)     # 32 hex digits
```

The labels are ASCII without a terminating NUL: `fleet-pair-v1` is 13
bytes, `fleet-pair-server-v1` is 20 bytes.

Field rules:

- `device_name`: shown in `fleet devices`. The daemon trims it, removes
  control characters and cuts it to 64 bytes. Empty becomes `"device"`.
- `device_platform`: free-form, for example `"macOS 26.0"`. Same cleaning.
- Pairing the same public key again replaces the stored device (same
  `device_id`).

### 7.5 Why confirming the fingerprint is required

The proof is an HMAC keyed with a 40-bit code. Anyone who receives a proof
can try all 2^40 codes offline in minutes. An active man in the middle on
the LAN could present its own certificate, collect the proof, crack the
code, and pair with the real daemon while the code is still valid. The
check that stops this is the user comparing the fingerprint the app observed
with the one `fleet pair` printed, **before** the proof is sent. The server
proof then protects the client against a daemon that does not know the code.

A fingerprint is accepted when the observed server id starts with what the
user confirmed; require at least 16 hex digits (64 bits). Show it in groups
of 4 digits. A good UI shows the first 16 digits prominently and asks the
user to compare them with the server's screen, or lets them type or scan
them.

Swift sketch of the crypto:

```swift
import CryptoKit

func normalizeCode(_ s: String) -> String? {
    let alphabet = Set("0123456789ABCDEFGHJKMNPQRSTVWXYZ")
    var out = ""
    for var c in s.uppercased() {
        if c == "-" || c == " " || c == "\t" { continue }
        if c == "O" { c = "0" }
        if c == "I" || c == "L" { c = "1" }
        guard alphabet.contains(c) else { return nil }
        out.append(c)
    }
    return out.count == 8 ? out : nil
}

func pairMAC(_ label: String, code: String, fp: Data, pub: Data) -> Data {
    let key = SymmetricKey(data: Data(code.utf8))           // normalized code
    var msg = Data(label.utf8); msg.append(fp); msg.append(pub)
    return Data(HMAC<SHA256>.authenticationCode(for: msg, using: key))
}

let pub = deviceKey.publicKey.rawRepresentation             // 32 bytes
let proof = pairMAC("fleet-pair-v1", code: normalized, fp: fp, pub: pub)
// ... send PairRequest, receive PairResponse, then:
var serverMsg = Data("fleet-pair-server-v1".utf8)
serverMsg.append(fp); serverMsg.append(pub)
guard HMAC<SHA256>.isValidAuthenticationCode(response.serverProof,     // constant time
                                             authenticating: serverMsg,
                                             using: SymmetricKey(data: Data(normalized.utf8)))
else { throw FleetError.serverProof }  // do not store the pin
```

## 8. Authentication

Every later connection authenticates with the device key.

```
Client                                                   Daemon
  |---- TCP connect, TLS 1.3 (pinned: hex(fp) == server id) ->|
  |<--- ServerMessage{id:0, hello{nonce, ...}} --------------|
  |     check protocol_version == 1, hello.server_id == hex(fp)
  |---- ClientMessage{id:1, auth: AuthRequest{               |
  |        device_id, signature}} -------------------------->|
  |                              look up device, verify sig |
  |<--- ServerMessage{id:1, auth: AuthResponse{device_name}}-|
```

```
auth_message = "fleet-auth-v1" || hello.nonce || fp        # 13 + 32 + 32 = 77 bytes
signature    = Ed25519-Sign(device private key, auth_message)   # 64 bytes
```

- `device_id` is the one from `PairResponse` (it always equals the value
  derived from the public key, section 7.4).
- `fp` is the fingerprint observed on this connection, not the stored one.
  They are equal because of the pin, but binding to the observed value keeps
  the signature useless on any other TLS session.
- CryptoKit's `Curve25519.Signing` is Ed25519:
  `try key.signature(for: authMessage)`. Apple's implementation may produce
  randomized signatures; the daemon accepts them (verification is standard
  Ed25519).
- Errors: an unknown or revoked `device_id` gets
  `Error{ERROR_CODE_UNAUTHENTICATED, "unknown or revoked device"}` and the
  connection is closed at once. Treat this as "unpaired": drop the stored
  server and ask the user to pair again. A bad signature gets
  `ERROR_CODE_UNAUTHENTICATED, "authentication failed"`.

```swift
var msg = Data("fleet-auth-v1".utf8)
msg.append(hello.nonce)
msg.append(fp)
var req = Fleet_V1_ClientMessage()
req.id = nextID()
req.auth = .with {
    $0.deviceID = stored.deviceID
    $0.signature = try deviceKey.signature(for: msg)
}
```

## 9. Storing the device key and paired servers

- **Device key.** Create one `Curve25519.Signing.PrivateKey()` per
  installation and keep it in the Keychain: store
  `privateKey.rawRepresentation` (the 32-byte seed) as a
  `kSecClassGenericPassword` item with
  `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`, so it never syncs or
  leaves the device. The Secure Enclave does not support Ed25519, so the key
  cannot live there. Reuse the same key for every server; the daemon only
  learns the public key.
- **Paired servers.** For each server store: server id (64 hex digits, the
  pin), device id, server name, last working address (`host:port`), and when
  it was paired. None of it is secret, but the server id is a security
  anchor: keep it where other apps cannot change it (the Keychain or the
  app's own container). The Go CLI stores the same fields in
  `~/.fleet/servers.json`.
- **Forgetting a server** only deletes local data. To remove the device on
  the server side, revoke it there (`RevokeDeviceRequest`, or
  `fleet devices revoke`).
- If the daemon's certificate changes (its `identity/` folder was deleted),
  the pin no longer matches and the device must pair again. Never replace a
  stored pin silently; show an explicit "the server's identity changed"
  error.

## 10. Requests, responses and pushed messages

- Every request carries a non-zero `id` chosen by the client. Use a
  per-connection counter starting at 1. The response, or an `Error`, carries
  the same `id`.
- Any request can fail with `ServerMessage{id, error: Error{code, message}}`
  instead of its normal response. `message` is for humans and may change;
  branch on `code`.
- The daemon handles one connection's requests **strictly one at a time, in
  order**, and its responses come back in request order. A slow request
  therefore delays everything sent after it on the same connection:
  `RunAgentRequest` can take seconds (git worktree creation, up to a 60 s
  limit; a `clone_url` clone, up to 10 minutes), `ListAdaptersRequest` runs each CLI's `--version` the first time
  (up to 3 s each), and `DetachRequest` waits up to 5 s. Use separate
  connections for independent work (section 13).
- Pushed messages have `id` 0 and can arrive at any time between responses:
  `Event` (after `SubscribeRequest`), `TerminalOutput` and `TerminalClosed`
  (while attached).
- `TerminalInput` and `TerminalResize` are fire-and-forget: send them with
  `id` 0. They never get a reply, not even an `Error`; failures (unknown
  attachment, read-only terminal, zero size) are only logged by the daemon.
  Do not send them with a non-zero id: success still sends nothing, so the
  request would never complete.
- A `ClientMessage` with no `msg` set, or with a message type this daemon
  does not know (for example from a newer client), gets
  `ERROR_CODE_INVALID_ARGUMENT`.
- The daemon never sends requests to the client and never pings. To detect
  a dead link, send `Ping` (for example every 30 s while in the foreground)
  and enable TCP keepalive. An authenticated connection has no idle timeout.

A small correlation table is enough:

```swift
actor FleetConnection {
    private var nextID: UInt64 = 1
    private var pending: [UInt64: CheckedContinuation<Fleet_V1_ServerMessage, Error>] = [:]

    func call(_ build: (inout Fleet_V1_ClientMessage) -> Void) async throws -> Fleet_V1_ServerMessage {
        var m = Fleet_V1_ClientMessage()
        build(&m)
        m.id = nextID; nextID += 1
        let id = m.id
        return try await withCheckedThrowingContinuation { cont in
            pending[id] = cont
            do { try send(m) } catch { pending.removeValue(forKey: id)?.resume(throwing: error) }
        }
    }

    /// Called by the read loop for every frame.
    func dispatch(_ m: Fleet_V1_ServerMessage) {
        if m.id != 0, let cont = pending.removeValue(forKey: m.id) {
            if case let .error(e)? = m.msg { cont.resume(throwing: FleetError.server(e.code, e.message)) }
            else { cont.resume(returning: m) }
            return
        }
        switch m.msg {
        case .event(let ev)?:           handleEvent(ev)
        case .terminalOutput(let o)?:   handleOutput(o)
        case .terminalClosed(let c)?:   handleClosed(c)
        default: break                  // hello is handled before the loop starts
        }
    }
}
```

When the connection drops, fail every pending continuation.

## 11. RPC reference

Every request below needs an authenticated connection (on TLS) and gets
`ERROR_CODE_UNAUTHENTICATED` otherwise. A connection whose device was
revoked is closed on its next request (the request gets
`UNAUTHENTICATED, "device revoked"` first if its id is non-zero).

Errors marked *(any)* can come from almost every request:
`ERROR_CODE_INTERNAL` for unexpected failures (I/O, tmux or git errors).

### Error codes

| Code | Meaning in this daemon |
|------|------------------------|
| `INTERNAL` (1) | unexpected failure; the message has details |
| `INVALID_ARGUMENT` (2) | malformed request, or an operation that does not fit the current state (agent not running, agent busy, not a directory, duplicate root) |
| `NOT_FOUND` (3) | unknown agent, root, adapter or device, not attached, or a missing directory |
| `ALREADY_EXISTS` (4) | agent name taken by a live agent; already attached to that agent on this connection |
| `UNAUTHENTICATED` (5) | not authenticated, unknown or revoked device, bad signature, or a message not allowed on this transport |
| `PERMISSION_DENIED` (6) | adapter not allowed in that root; input to a read-only terminal (not reported, see section 10) |
| `OUTSIDE_ROOTS` (7) | path outside every root, or escaping its root (`..`, symlinks) |
| `ADAPTER_UNAVAILABLE` (8) | the agent CLI is not installed, or the adapter could not build its command |
| `DIRECTORY_BUSY` (9) | reserved; no longer sent (several agents may be pinned to one directory) |
| `PAIRING_FAILED` (10) | pairing code wrong, expired or used up |
| `UNSUPPORTED_PROTOCOL` (11) | reserved; not sent by v1 daemons |

### Handshake and liveness

| Request | Response | Notes and errors |
|---------|----------|------------------|
| `Ping` | `Pong` | Allowed before authentication. Never fails. |
| `PairRequest` | `PairResponse` | Section 7. `INVALID_ARGUMENT` (key size), `PAIRING_FAILED`, `UNAUTHENTICATED` on the local socket. |
| `AuthRequest` | `AuthResponse{device_name}` | Section 8. `UNAUTHENTICATED`, also on the local socket. |

### Daemon, adapters, roots

**`GetInfoRequest` → `GetInfoResponse`**: `server_id`, `server_name`,
`daemon_version`, `hostname`, `os` and `arch` (Go names: `linux`,
`darwin`, `amd64`, `arm64`), `started_at_ms` (Unix ms). No errors.

**`ListAdaptersRequest` → `ListAdaptersResponse`**: every registered
adapter (v1: `claude`, `codex`, `shell`), whether its CLI was found, its
path and version, `unavailable_reason` when not found, and
`capabilities`:

| Capability | claude | codex | shell |
|------------|--------|-------|-------|
| `activity_state` (hooks report WORKING / IDLE / NEEDS_INPUT) | yes | yes | no |
| `initial_prompt` (`RunAgentRequest.prompt` is used) | yes | yes | no |
| `resume` | no | no | no |
| `initial_images` (`RunAgentRequest.images` are used) | yes | yes | no |

Adapters that offer a choice of model list `models` (`id`, `label`, and
the `efforts` ids each runs at: none for a model without effort levels)
and `efforts` (`id`, `label`, least first), for `RunAgentRequest.model`
and `.effort`. Claude Code offers `opus`, `fable`, `sonnet` and `haiku` at
`low` to `max` and `ultracode`; Codex the models of its own
`models_cache.json`; the shell none.

Detection results are cached by the daemon. No errors.

**`ListRootsRequest` → `ListRootsResponse`**: configured roots, each with
`name`, symlink-resolved absolute `path`, `adapters` (empty = all
allowed) and `trust`. With `trust`, the daemon answers the agent CLI's
"Do you trust this folder?" prompt for agents started in the root (see
[Agent state machine](#14-agent-state-machine)); a client should say so
when offering the option, since a trusted project may run its own hooks
and MCP servers. No errors.

**`AddRootRequest{path, name, adapters, trust}` → `AddRootResponse{root}`**

- `path` must be absolute on the daemon host; it is stored
  symlink-resolved. `name` defaults to the folder's base name, made unique
  with `-2`, `-3`, ...
- Saved to `config.toml` and broadcast as `Event.roots_changed`.
- Errors: `INVALID_ARGUMENT` (relative path, not a directory, path already a
  root, name taken, name containing `/` or `\`); `NOT_FOUND` (path does not
  exist, or an unknown adapter id in `adapters`).

**`RemoveRootRequest{name}` → `RemoveRootResponse`**

- Running agents in that root keep running. Broadcast as
  `Event.roots_changed`.
- Errors: `NOT_FOUND` (no such root).

**`BrowseRequest{root, path, include_hidden}` → `BrowseResponse{root, path, entries}`**

- Lists the sub-directories of `root`/`path`. `path` is relative; `""` or
  `"."` is the root itself. Files are never listed.
- Entries are sorted by name, at most 2000. Hidden (`.`-prefixed) folders
  are skipped unless `include_hidden`. A symlink is listed only if it
  points to a directory inside the same root.
- `is_git_repo` is true for a work-tree root (a folder containing `.git`).
  `agent_count` counts live agents whose requested path is that folder or
  below it.
- The response `path` is normalized (`""` for the root, `/`-separated).
- Errors: `INVALID_ARGUMENT` (empty root, not a directory); `NOT_FOUND`
  (unknown root, missing directory); `OUTSIDE_ROOTS` (absolute path, `..`,
  or a symlink leaving the root).

### Agents

**`ListAgentsRequest{include_finished}` → `ListAgentsResponse{agents}`**:
live agents, plus finished ones (EXITED, FAILED) if `include_finished`.
Sorted by creation time. The daemon keeps the 200 most recent finished
agents. No errors.

**`RunAgentRequest` → `RunAgentResponse{agent}`**

Target, one of:

- `root` + `path` (relative inside the root; `""` is the root itself), or
- `absolute_path` (used when `root` is empty); the most specific root that
  contains it is chosen, or
- `clone_url`: a git repository the daemon clones (see `ISOLATION_CLONE`).
  `https://…`, `ssh://…`, scp-like `git@host:org/repo`, and the shorthands
  `owner/repo` (GitHub) and `host.tld/owner/repo` (https) are accepted;
  local paths, `file://` and other transports are refused. The daemon
  clones with its own git credentials. Roots do not apply.

The directory is fully symlink-resolved and must exist and lie inside the
root.

Isolation:

- `ISOLATION_UNSPECIFIED`: `WORKTREE` if the directory is inside a git
  repository and `default_isolation` in `config.toml` is not `"pinned"`,
  otherwise `PINNED`.
- `ISOLATION_PINNED`: run directly in the directory. Several agents may be
  pinned to the same directory; they share its files.
- `ISOLATION_WORKTREE`: the daemon creates a git worktree of the repository
  under `$FLEET_HOME/worktrees/<repo>-<hash>/<name>` on a new branch
  (`branch`, default `fleet/<name>`) and runs the agent in the same
  sub-directory of the worktree that was requested. `Agent.path` is the
  requested directory, `Agent.cwd` is the directory inside the worktree.
- `ISOLATION_CLONE`: only with `clone_url` (which implies it). The daemon
  clones the repository into `$FLEET_HOME/clones/<name>` (submodules are not
  cloned) and checks out `branch` (default `fleet/<name>`): the remote
  branch of that name if there is one, else a new branch from the default
  branch. `Agent.path` and `Agent.cwd` are the clone, `Agent.clone_url` the
  normalized URL.

Sandbox (`sandbox`):

- `SANDBOX_UNSPECIFIED`: `[sandbox] default` in `config.toml`, `NONE`
  unless set.
- `SANDBOX_NONE`: the agent runs on the daemon host.
- `SANDBOX_DOCKER`: the agent runs in a Docker container (the agent CLI
  comes from the image, not the host). The tmux pane runs `docker run -it`,
  so attaching, `SendText`, states and exit codes work as without a
  sandbox. The container sees only the agent's checkout at its host path
  (worktree, clone, or for `PINNED` the folder, or the whole repository
  when the folder is inside one), the repository's shared git directory
  (read-write, so the agent can commit; its `config` and `hooks` read-only),
  a persistent sandbox home directory, and a socket that only accepts hook
  events for this agent. Worktrees and clones of sandboxed agents are
  created under `[sandbox] dir` instead of `$FLEET_HOME`. Folder trust
  prompts are always pre-answered in a sandbox. The container is removed
  when the agent ends or is killed. `Agent.sandbox` is never
  `UNSPECIFIED`.

Other fields:

- `name`: optional; generated as `<adapter>-<folder>-<n>` when empty. Must
  be unique among live agents (and not equal a live agent's id). Cleaned
  like device names (64 bytes max).
- `prompt`: initial prompt, used when the adapter has `initial_prompt`.
- `images`: images (PNG, JPEG, GIF or WebP, up to 3.5 MiB each) attached
  to `prompt`, which they need; only for adapters with `initial_images`
  (`INVALID` otherwise). They are saved in the agent's state dir; Claude
  Code gets them as `@<path>` mentions appended to the prompt, Codex as
  `--image=<path>`. The whole request must still fit a frame (4 MiB).
- `model`, `effort`: optional model and reasoning effort, ids from
  `AdapterInfo.models` and `.efforts` (`model` may also be any name the
  CLI takes, such as `claude-opus-5-5`). Empty leaves them to the CLI's own
  settings. The adapter passes them on the command line (`--model` and
  `--effort`, `-m` and `-c model_reasoning_effort=…`), after `extra_args`.
- `extra_args`: appended to the agent CLI's command line, after the
  per-adapter `args` from `config.toml`.
- `cols`, `rows`: initial terminal size, default 200x50, capped at 1000.

Behaviour:

- The agent is registered as `STARTING` right away (an `agent_upserted`
  event is broadcast), then the worktree and the tmux session are created.
  The response is sent once the session exists; the agent is then
  `RUNNING` (or already `IDLE`/`WORKING` if a hook arrived that fast).
- Starting continues even if the requesting client disconnects.
- If starting fails, the agent is removed again (`agent_removed` event) and
  the request fails. A start failure never leaves a `FAILED` agent; `FAILED`
  is for agents that die right after starting (section 14).

Errors: `NOT_FOUND` (unknown adapter, unknown root, missing directory);
`ADAPTER_UNAVAILABLE` (CLI not installed, or the adapter could not build its
command; for `SANDBOX_DOCKER`: Docker unreachable, image missing, or Docker
cannot mount `[sandbox] dir`; the clone failed); `INVALID_ARGUMENT` (no
target, `clone_url` together with a folder or a non-`CLONE` isolation,
unsupported `clone_url`, relative `absolute_path`, not a directory,
`WORKTREE` outside a git repository, `CLONE` without `clone_url`, invalid
name, unknown isolation or sandbox, worktree creation failed, a model or
effort the adapter does not offer);
`OUTSIDE_ROOTS`; `PERMISSION_DENIED` (adapter not allowed in that root);
`ALREADY_EXISTS` (name taken);
`INTERNAL` (tmux failed).

**`KillAgentRequest{agent, remove_worktree, force, forget}` → `KillAgentResponse{agent, worktree_kept, worktree_kept_reason}`**

- `agent` is an id or a name. Names prefer the live agent, then the newest
  finished one.
- A live agent's tmux session is killed (and its container removed, for
  `SANDBOX_DOCKER`). The agent becomes `EXITED` with detail `"killed"` (or
  the real exit status if it had already died).
  Killing a finished agent is allowed; it just does the worktree and forget
  steps.
- `remove_worktree`: also remove the agent's worktree (branch is kept) or
  clone. It is kept, with `worktree_kept = true` and a reason, if it has
  uncommitted changes, or for a clone, commits that are on no remote
  branch, unless `force` is set. `Agent.worktree` is the worktree or clone
  until it is removed, empty after.
- `forget`: remove the agent from history (`agent_removed` event).
- Errors: `INVALID_ARGUMENT` (empty `agent`, or the agent is busy starting
  or being killed; retry later); `NOT_FOUND`; `INTERNAL` (tmux failed).

**`SendTextRequest{agent, text, submit}` → `SendTextResponse`**

- Types `text` literally into the agent's terminal (tmux `send-keys -l`),
  then presses Enter if `submit`. Works without attaching; good for "send a
  prompt from the phone".
- Errors: `INVALID_ARGUMENT` (empty `agent`, agent not running);
  `NOT_FOUND`.

**`AttachImageRequest{agent, data}` → `AttachImageResponse{path}`**

- Attaches an image to the prompt the agent is composing, as if pasted:
  the daemon saves `data` in the agent's state dir and pastes the file's
  path, which Claude Code and Codex show as `[Image #n]`. It answers once
  the agent has taken the paste in (its screen changed and settled, at
  most 3 s), so a `SendTextRequest` sent after the response lands after
  the image. Send one request per image, then the text.
- `data`: PNG, JPEG, GIF or WebP (detected from the bytes), at most
  3.5 MiB, so a request fits a frame. `path` is on the daemon's host.
- Errors: `INVALID_ARGUMENT` (empty `agent`, agent not running, not an
  image, too large); `NOT_FOUND`.

**`MergePullRequestRequest{agent, pull_request, method, delete_branch}` →
`MergePullRequestResponse{pull_request}`**

- `Agent.pull_requests` lists the GitHub pull requests an agent created:
  the daemon reads the agent's transcript for a `gh pr create` (or a GitHub
  MCP tool named `…create_pull_request`) and takes the pull request URLs
  its output shows, also from transcripts written before the daemon
  looked. It looks up each one with the GitHub CLI (`gh`) on the daemon
  host, logged in as the daemon user: right away, then every minute while
  it is open and the agent runs, every 10 minutes after the agent ended,
  never once merged or closed. Each change comes as an `agent_upserted`
  event. `detail` says why a lookup failed (no `gh`, not logged in, no
  access); the rest is the last status seen.
- This request merges one: `pull_request` is its URL or number, or empty
  for the agent's only open one. `method` `UNSPECIFIED` takes the first of
  `PullRequest.merge_methods` (squash, merge, rebase, as the repository
  allows), squash if those are unknown. A draft is marked ready for
  review first. `delete_branch` deletes the head branch on GitHub; the
  agent's local branch and worktree are not touched. The response is the
  pull request as looked up after merging. The daemon may take up to 90 s.
- Errors: `INVALID_ARGUMENT` (merged or closed already, a method the
  repository does not allow, several open pull requests and none named,
  or what GitHub refused: failing required checks, missing reviews,
  conflicts; the message is gh's); `NOT_FOUND` (agent, or no such / no
  open pull request); `ADAPTER_UNAVAILABLE` (no `gh` on the daemon host).

**`GitRequest{agent, action, pull_mode, from_base, force}` →
`GitResponse{status, output}`**

- `Agent.git` is the git state of the agent's checkout (its worktree,
  clone or pinned directory): branch, HEAD, upstream with `ahead` /
  `behind`, the remote's default branch (`base`, e.g. `origin/main`) with
  `base_ahead` / `base_behind`, uncommitted (`changed`) and conflicted
  files, an operation in progress, and when it was last fetched. The
  daemon reads it every 20 s while the agent runs (no network: the counts
  are as of the last fetch) and on this request; each change comes as an
  `agent_upserted` event. Unset outside a git repository.
- `action`: `STATUS` (or `UNSPECIFIED`) reads it; `FETCH` fetches the
  upstream's remote (else `origin`); `PULL` fetches, then brings the
  upstream into the branch, or `base` with `from_base` or when the branch
  has no upstream: `pull_mode` `FF_ONLY` (default), `REBASE` or `MERGE`,
  all with `--autostash`. A merge or rebase that conflicts is aborted, so
  the checkout is as before. `PUSH` pushes the branch to the same name on
  its remote and makes that its upstream; `force` uses
  `--force-with-lease`. `status` is the checkout after the action,
  `output` git's last lines.
- Git runs on the daemon host as the daemon user, with their credentials,
  in its own session without a terminal: it never prompts, missing
  credentials fail. Actions on one checkout run one at a time.
- Errors: `INVALID_ARGUMENT` (pull while the agent is `WORKING`, detached
  HEAD, no remote, nothing to pull from, diverged for a fast-forward,
  conflicts, a rejected push, or git's own message); `NOT_FOUND`;
  `ADAPTER_UNAVAILABLE` (no `git` on the daemon host).

**`SubscribeRequest` → `SubscribeResponse`**, then pushed events. See
section 12.

### Terminal

See section 13 for the flow.

| Request | Response | Errors |
|---------|----------|--------|
| `AttachRequest{agent, cols, rows, read_only}` | `AttachResponse{agent_id}` | `INVALID_ARGUMENT` (empty agent, agent not running); `NOT_FOUND`; `ALREADY_EXISTS` (already attached to it on this connection) |
| `TerminalInput{agent_id, data}` (id 0) | none | none reported |
| `TerminalResize{agent_id, cols, rows}` (id 0) | none | none reported |
| `DetachRequest{agent_id}` | `DetachResponse` | `NOT_FOUND` (not attached on this connection) |

### Devices

**`CreatePairingCodeRequest{ttl_seconds}` → `CreatePairingCodeResponse{code, expires_at_ms, server_id}`**:
`ttl_seconds` 0 means 300, anything larger is cut to 300. Invalidates any
earlier code. Show `code` and `server_id` (the fingerprint) to the user.
No errors.

**`ListDevicesRequest` → `ListDevicesResponse{devices}`**: paired devices
with `paired_at_ms`, `last_seen_at_ms` (updated on every authenticated
request), and `connected` (has an authenticated connection right now). No
errors.

**`RevokeDeviceRequest{device}` → `RevokeDeviceResponse`**

- `device` is a full device id, a unique id prefix of at least 6 hex
  digits, or a device name.
- The device is deleted and all its connections are closed after the
  response is sent. A device may revoke itself.
- Errors: `NOT_FOUND`.

### Local only

**`HookEvent` → `HookResponse`**: sent by `fleet hook` from inside an
agent's process tree. On TLS it fails with `UNAUTHENTICATED`. Clients never
send it. With `wait` set (`fleet hook --wait`), a hook that asks the user
something the dashboard can answer (Claude's `AskUserQuestion`) gets its
response only once the question is answered on the dashboard, answered in
the terminal, or gone; `output` is what the hook prints for the agent CLI.

## 12. Events (Subscribe)

```
Client                                  Daemon
  |-- {id:7, subscribe} ------------------>|
  |<- {id:7, subscribe: SubscribeResponse}-|
  |<- {id:0, event: agent_upserted A} -----|   snapshot: one per known agent,
  |<- {id:0, event: agent_upserted B} -----|   live and finished, by creation time
  |<- {id:0, event: snapshot_done} --------|
  |<- {id:0, event: agent_upserted B'} ----|   live changes from here on
  |<- {id:0, event: roots_changed} --------|
  |<- {id:0, event: agent_removed "a1b2c3"}|
```

- `agent_upserted`: the full `Agent` whenever anything about it changes
  (state, detail, exit code, session id, attached clients). Replace your
  copy by `id`; do not merge fields.
- `agent_removed`: the agent id; it was forgotten, trimmed from history, or
  its start failed.
- `roots_changed`: the full new root list after `AddRoot` / `RemoveRoot`.
  Roots are **not** part of the snapshot: call `ListRootsRequest` once
  after subscribing.
- The snapshot includes finished agents (the same set as
  `ListAgentsRequest{include_finished: true}`). Filter in the UI.
- A repeated `SubscribeRequest` on the same connection gets a new
  `SubscribeResponse`, keeps the single event stream, and queues a fresh
  snapshot plus `snapshot_done` on it. Use this to resynchronize.
- Events may arrive before the response of the request that caused them.
  For example, the `STARTING` upsert of a new agent usually arrives before
  `RunAgentResponse`. Always key by agent id.
- **Keep reading.** Each subscriber has a buffer of (number of agents +
  256) events. A client that falls further behind is dropped, and the
  **whole connection is closed**. Reconnect and subscribe again.
- There is no unsubscribe; close the connection.

## 13. Terminal streaming

Attaching starts a real tmux client on the daemon host, inside a PTY of the
requested size, attached to the agent's session. Its output is streamed to
you as raw terminal bytes, exactly what a terminal emulator would receive
(`TERM=xterm-256color`: UTF-8, escape sequences, alternate screen, colors,
mouse modes). Feed them unmodified into a terminal emulator:
[SwiftTerm](https://github.com/migueldeicaza/SwiftTerm) on Apple platforms,
or libghostty.

```
Client (terminal connection)                       Daemon
  |-- {id:1, auth ...} / response --------------------->|
  |-- {id:2, attach{agent:"api-fix", cols:120,          |
  |          rows:40, read_only:false}} --------------->|  tmux attach in a PTY
  |<- {id:2, attach{agent_id:"a1b2c3"}} ---------------|
  |<- {id:0, terminal_output{a1b2c3, bytes}} ----------|  full redraw first,
  |<- {id:0, terminal_output{a1b2c3, bytes}} ----------|  then live output
  |-- {id:0, terminal_input{a1b2c3, "ls\r"}} --------->|  keystrokes
  |-- {id:0, terminal_resize{a1b2c3, 100, 30}} ------->|  window resized
  |-- {id:3, detach{a1b2c3}} ------------------------->|
  |<- {id:0, terminal_closed{a1b2c3, "detached"}} -----|
  |<- {id:3, detach{}} --------------------------------|
```

Rules:

- `AttachRequest.agent` is an id or a name. Use the returned `agent_id` in
  every later terminal message. No `TerminalOutput` is sent before the
  `AttachResponse`.
- `cols` and `rows` 0 mean 200x50; values are capped at 1000.
- tmux redraws the whole screen when a client attaches and after a resize,
  so the first output gives you the current screen. There is no separate
  scrollback transfer; tmux keeps 50000 lines of history in the session,
  reachable with tmux copy mode inside the stream.
- `TerminalOutput.data` chunks are at most 32 KiB and may split UTF-8 and
  escape sequences anywhere. Terminal emulators handle this; do not decode
  chunks as strings.
- `TerminalInput.data` is what the user typed, as bytes (Enter is `\r`).
  Keep chunks at or below 32 KiB, like the Go client. Input to a read-only
  attachment is dropped silently.
- `TerminalResize` changes the PTY size. The tmux option `window-size
  latest` makes the agent's window follow the most recently active client,
  so when several clients with different sizes watch the same agent, the
  one that last typed or resized wins. Send a resize whenever your view
  changes size; `cols` or `rows` of 0 are ignored.
- `read_only: true` attaches a tmux client with `-r`: you see everything and
  cannot type.
- One attachment per agent per connection (`ALREADY_EXISTS` otherwise). One
  connection can hold attachments to several agents.
- `TerminalClosed{agent_id, reason}` is pushed exactly once when an
  attachment ends: `"detached"` after your `DetachRequest` (sent before the
  `DetachResponse`), `"terminal ended"` when the tmux client exited for
  another reason, typically because the agent exited and its session was
  cleaned up. When the connection closes, its attachments end silently.
- Detaching or disconnecting never stops the agent. Use
  `KillAgentRequest` for that.
- `Agent.attached_clients` counts every tmux client on the session (fleet
  attachments and `tmux attach` on the server), refreshed about every
  second.

**Use one connection per open terminal**, plus one control connection for
requests and events. Each connection authenticates on its own (with a new
nonce). Reasons:

- Requests on a connection run one at a time (section 10). A `RunAgent` on
  the same connection would freeze typing for seconds.
- A busy terminal fills the connection, which can make an event subscriber
  on it fall behind and get the connection closed (section 12).
- Closing a terminal is then just closing its connection.

SwiftTerm wiring (macOS):

```swift
import SwiftTerm

final class AgentTerminalController: NSViewController, TerminalViewDelegate {
    let terminal = TerminalView(frame: .zero)
    let conn: FleetConnection        // dedicated connection, already authenticated
    var agentID = ""

    func start(agent: String) async throws {
        terminal.terminalDelegate = self
        let t = terminal.getTerminal()
        let resp = try await conn.call {
            $0.attach = .with { $0.agent = agent; $0.cols = UInt32(t.cols); $0.rows = UInt32(t.rows) }
        }
        agentID = resp.attach.agentID
        // In FleetConnection.dispatch: .terminalOutput(o) ->
        //     terminal.feed(byteArray: ArraySlice(o.data))   // any thread, not from a delegate callback
    }

    func send(source: TerminalView, data: ArraySlice<UInt8>) {
        conn.post { $0.terminalInput = .with { $0.agentID = agentID; $0.data = Data(data) } }  // id 0
    }

    func sizeChanged(source: TerminalView, newCols: Int, newRows: Int) {
        conn.post { $0.terminalResize = .with {
            $0.agentID = agentID; $0.cols = UInt32(newCols); $0.rows = UInt32(newRows) } }
    }

    func setTerminalTitle(source: TerminalView, title: String) {}
    func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {}
    func scrolled(source: TerminalView, position: Double) {}
    func requestOpenLink(source: TerminalView, link: String, params: [String: String]) {}
    func clipboardCopy(source: TerminalView, content: Data) {}
    func rangeChanged(source: TerminalView, startY: Int, endY: Int) {}
}
```

`conn.post` stands for "send with id 0, no reply expected".

## 14. Agent state machine

```
                RunAgent
                   │
                   ▼
              ┌──────────┐  start failed: agent removed (agent_removed),
              │ STARTING │  RunAgent returns an error
              └────┬─────┘
                   │ tmux session created
                   ▼
              ┌──────────┐
              │ RUNNING  │  no hook has reported yet (or the adapter has none)
              └────┬─────┘
                   │ hooks (adapters with activity_state)
                   ▼
   ┌─────────┐  ◀──────▶  ┌──────┐  ◀──────▶  ┌─────────────┐
   │ WORKING │            │ IDLE │            │ NEEDS_INPUT │
   └─────────┘  ◀───────────────────────────▶ └─────────────┘
        │  any live state
        ▼
   process ended / killed / session gone
        │
        ├── exit status != 0 or signal, within ~3 s of start ──▶ FAILED
        └── otherwise ────────────────────────────────────────▶ EXITED
```

- Live states: `STARTING`, `RUNNING`, `WORKING`, `IDLE`, `NEEDS_INPUT`.
  Final states: `EXITED`, `FAILED`. A final state never changes again.
- Hooks can move a live agent between `RUNNING`, `WORKING`, `IDLE` and
  `NEEDS_INPUT` in any order. They can never end an agent: only tmux decides
  that. `state_detail` carries a short hint, such as the tool waiting for
  permission (`NEEDS_INPUT`) or the last API error.
- Adapter mappings in v1:
  - **claude**: `SessionStart` → IDLE; `UserPromptSubmit`, `PreToolUse`,
    `PostToolUse`, `PermissionDenied` → WORKING; `PermissionRequest` →
    NEEDS_INPUT; `Notification` → IDLE for the "waiting for your input"
    notice, ignored for permission prompts (`PermissionRequest` reports
    them), otherwise NEEDS_INPUT; `PostToolUseFailure` → IDLE if it was an
    interrupt, else WORKING; `Stop`, `StopFailure` → IDLE.
    NEEDS_INPUT opens a dialog for the tool call it asks about (and the
    subagent asking, if any). Only later hooks of that call close it, or
    the end of the turn or subagent: tool calls go on meanwhile, in
    parallel or in background subagents. Subagents' hooks never set the
    agent's own state. While several dialogs are open, `state_detail`
    describes the oldest, which is the one on screen. Some dialogs close
    without a hook (a refused permission fires none): the daemon drops
    them once the agent's screen shows no dialog, and the agent returns to
    the state its own hooks last reported.
  - **codex**: `SessionStart` → WORKING (IDLE after `/clear`);
    `UserPromptSubmit`, `PreToolUse`, `PostToolUse` → WORKING;
    `PermissionRequest` → NEEDS_INPUT; `Stop` → IDLE; legacy `notify`
    turn-complete → IDLE, approval → NEEDS_INPUT.
  - **shell**: no hooks; stays `RUNNING`.
- **Startup dialogs.** Claude Code and Codex ask whether to trust a folder
  the first time they run there, and Codex may ask to review hooks. No hook
  fires until the user answers in the terminal. The daemon recognizes these
  dialogs on screen during the first two minutes after start and reports
  `NEEDS_INPUT` with a `state_detail` such as `"Claude asks whether to
  trust this folder; answer in its terminal"`. When the dialog goes away
  the agent returns to `RUNNING` until its hooks report more. The client
  should offer to attach. In roots with `trust` the folder trust dialog
  does not appear. A CLI whose dialog text changed shows `RUNNING` while it
  waits, so a client should still present `RUNNING` of an
  `activity_state` adapter as "running, may be waiting in the terminal".
- The daemon checks tmux every second. When the agent process ends, the
  daemon records its exit and kills the session:
  - `EXITED`, `has_exit_code = true`, `exit_code` = the status, detail
    `"exited with status N"`. A process killed by signal N reports
    `exit_code = 128 + N` and detail `"killed by signal N"`.
  - `FAILED` instead, if it ended with a non-zero status or a signal
    within about 3 s of starting. `state_detail` then holds the last lines
    of its terminal output (up to 20 lines, 2000 bytes), usually the error
    message.
  - `EXITED` with detail `"killed"` after `KillAgentRequest`, `"session
    disappeared"` if the tmux session vanished without an exit status (for
    example killed by hand), or `"session disappeared while the daemon was
    stopped"`. No exit code in these cases.
- Agents survive a daemon restart and are re-adopted with the same id; the
  client just reconnects.
- `updated_at_ms` changes on every upsert. `session_id` is the agent CLI's
  own session id where known (Claude: set at launch; Codex: from hooks).
- `tmux_session` is `fleet-<id>` on the daemon's tmux socket (`tmux -L
  fleet` by default). On the server, `tmux -L fleet attach -t fleet-<id>`
  attaches directly.

## 15. Connection rules and limits

| Rule | Value |
|------|-------|
| Max frame | 4 MiB; 64 KiB until paired or authenticated |
| TLS handshake timeout | 15 s |
| Time allowed to pair or authenticate after the handshake | 60 s, then the connection is closed |
| Unauthenticated TLS connections at a time | 64; more are closed immediately |
| Failed `PairRequest`/`AuthRequest` per connection | the 3rd failure gets its `Error` and then the connection is closed; each failure is delayed about 300 ms |
| `AuthRequest` for an unknown device | `Error{UNAUTHENTICATED}` and the connection is closed at once |
| Failed attempts per pairing code | 5, across all connections; then the code is invalid |
| Pairing code lifetime | at most 300 s |
| Subscriber backlog | number of agents + 256 events, then the connection is closed |
| Terminal output chunk | at most 32 KiB |
| Terminal size | default 200x50, capped at 1000x1000 |
| Finished agents kept | 200 most recent |
| Revoked device | all its connections are closed; a connection that authenticated concurrently is closed on its next request |
| Daemon shutdown | every connection is closed; agents keep running |

`PairRequest` and `AuthRequest` on the local socket fail with
`UNAUTHENTICATED` because they are not allowed on that transport.

## 16. Worked examples

### 16.1 First launch: discover and pair

1. `NWBrowser` finds `studio` with TXT `v=1`, `id=3b4405b1be5c9a1f...`.
2. The user picks it and types the code `abcd-efgh` shown by `fleet pair`
   on the server. `normalizeCode` gives `ABCDEFGH`.
3. Connect with `fleetParameters(pin: nil, ...)`. The verify block records
   `fp`.
4. Receive `ServerHello`. Check `protocol_version == 1` and
   `server_id == fp.hex`.
5. Show `3b44 05b1 be5c 9a1f ...` and ask: "Does this match the fingerprint
   on the server?". Abort unless the user confirms.
6. Send:
   ```
   ClientMessage {
     id: 1
     pair { device_name: "Flo's MacBook", device_public_key: <32 B>,
            proof: HMAC-SHA256("ABCDEFGH", "fleet-pair-v1" || fp || pub),
            device_platform: "macOS 26.0" }
   }
   ```
7. Receive `ServerMessage{id: 1, pair: {device_id, server_proof, server_id,
   server_name}}`. Verify `server_proof`. Store `{server_id, device_id,
   name, address}`.
8. The same connection is now authenticated. Continue with 16.2 step 3.

### 16.2 Normal session: dashboard

1. Connect with `fleetParameters(pin: stored.serverID, ...)` to the stored
   address; on failure browse for a service with that TXT `id`.
2. Receive `ServerHello`, check it, send `AuthRequest` (section 8), receive
   `AuthResponse`.
3. `{id:2, subscribe}`, then read the snapshot until `snapshot_done`, and
   show the agent list.
4. `{id:3, list_roots}` and `{id:4, list_adapters}` for the "new agent"
   sheet; `{id:5, browse{root: "code", path: ""}}` for the folder picker.
5. Keep reading events; ping every 30 s.

### 16.3 Start an agent and open its terminal

On the control connection:

```
-> {id:6, run_agent{adapter:"claude", root:"code", path:"api",
                    prompt:"fix the flaky test", cols:120, rows:40}}
<- {id:0, event{agent_upserted{id:"a1b2c3", name:"claude-api-1", state:STARTING, ...}}}
<- {id:0, event{agent_upserted{id:"a1b2c3", state:RUNNING, cwd:"/home/flo/.fleet/worktrees/api-1f2e3d4c/claude-api-1",
                               isolation:WORKTREE, branch:"fleet/claude-api-1", ...}}}
<- {id:6, run_agent{agent{id:"a1b2c3", state:RUNNING, ...}}}
<- {id:0, event{agent_upserted{id:"a1b2c3", state:WORKING, ...}}}     # from Claude's hooks
```

Then open a second connection, authenticate, and attach:

```
-> {id:2, attach{agent:"a1b2c3", cols:120, rows:40}}
<- {id:2, attach{agent_id:"a1b2c3"}}
<- {id:0, terminal_output{agent_id:"a1b2c3", data:<VT bytes>}}  ... feed into SwiftTerm
-> {id:0, terminal_input{agent_id:"a1b2c3", data:"y"}}
```

When the user closes the terminal window, close that connection (or send
`DetachRequest` first). The agent keeps running.

### 16.4 Send a prompt without attaching

```
-> {id:9, send_text{agent:"claude-api-1", text:"now run the tests", submit:true}}
<- {id:9, send_text{}}
```

### 16.5 Stop an agent and clean up its worktree

```
-> {id:10, kill_agent{agent:"a1b2c3", remove_worktree:true}}
<- {id:0, event{agent_upserted{id:"a1b2c3", state:EXITED, state_detail:"killed"}}}
<- {id:10, kill_agent{agent{...EXITED...}, worktree_kept:true,
         worktree_kept_reason:"worktree has uncommitted changes (use force to remove it anyway)"}}
```

Ask the user, then retry with `force: true`, or leave the worktree for them
to commit.

## 17. Test vectors

These values are asserted by `TestProtocolVectors`
(`internal/identity/vectors_test.go`) and `TestFrameVectors`
(`internal/wire/vectors_test.go`). All byte values are hex.

### Inputs

| Name | Value |
|------|-------|
| code (as typed) | `ABCD-EFGH` (also `abcd efgh`, `AB-CD-EF-GH`) |
| normalized code / HMAC key | `ABCDEFGH` = `4142434445464748` |
| look-alike check | `0I1L-OOAB` normalizes to `011100AB` |
| `fp` (fake certificate SHA-256) | `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f` |
| Ed25519 seed (private key, 32 B; CryptoKit `rawRepresentation`) | `a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf` |
| `ServerHello.nonce` | `404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f` |

### Outputs

| Name | Value |
|------|-------|
| device public key | `4fd099ccd47d7893dfe9ec24414ecb0d9b5420232aad30d91c465be33cbe65c4` |
| device id | `a18112b0b7b4225ff30527e0f7cf7a1e` |
| pair MAC input (`"fleet-pair-v1" \|\| fp \|\| pub`) | `666c6565742d706169722d7631` `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f` `4fd099ccd47d7893dfe9ec24414ecb0d9b5420232aad30d91c465be33cbe65c4` |
| `PairRequest.proof` | `69d5f9d7bb63848f45ed99cb5d05b3d77a4782452df8f1fbd8417de0d512aebf` |
| `PairResponse.server_proof` | `a6bc3eaf7fd3f1df1f6929e408676c03ca15ddf2e8a4d445ff3cc9f72f55d0c3` |
| auth message (77 bytes) | `666c6565742d617574682d7631` `404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f` `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f` |
| Ed25519 signature (RFC 8032, deterministic) | `6ee459e68222c2c84442f826baad8ea9b87208b0284344ac4ca211e74bc1e3828ebf12494e4908f9b3000864d72ee98df2e7c28689c4342585e10bf0ac4cdf0d` |

(The multi-part values are one byte string, split at the `||` boundaries
for readability.)

In a Swift unit test, check the public key, device id, proofs and auth
message bytes for equality. For the signature, check that
`publicKey.isValidSignature(vectorSignature, for: authMessage)` is true and
that your own signature also verifies. Do not compare your signature
bytes: CryptoKit on Apple platforms may randomize Ed25519 signatures.

```swift
func testVectors() throws {
    let seed = Data(hex: "a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf")
    let key = try Curve25519.Signing.PrivateKey(rawRepresentation: seed)
    let pub = key.publicKey.rawRepresentation
    XCTAssertEqual(pub.hex, "4fd099ccd47d7893dfe9ec24414ecb0d9b5420232aad30d91c465be33cbe65c4")
    XCTAssertEqual(Data(SHA256.hash(data: pub)).prefix(16).hex, "a18112b0b7b4225ff30527e0f7cf7a1e")

    let fp = Data(0x00...0x1f)
    let nonce = Data(0x40...0x5f)
    let code = try XCTUnwrap(normalizeCode("ABCD-EFGH"))
    XCTAssertEqual(pairMAC("fleet-pair-v1", code: code, fp: fp, pub: pub).hex,
                   "69d5f9d7bb63848f45ed99cb5d05b3d77a4782452df8f1fbd8417de0d512aebf")
    XCTAssertEqual(pairMAC("fleet-pair-server-v1", code: code, fp: fp, pub: pub).hex,
                   "a6bc3eaf7fd3f1df1f6929e408676c03ca15ddf2e8a4d445ff3cc9f72f55d0c3")

    var msg = Data("fleet-auth-v1".utf8); msg.append(nonce); msg.append(fp)
    let sig = Data(hex: "6ee459e68222c2c84442f826baad8ea9b87208b0284344ac4ca211e74bc1e382"
                      + "8ebf12494e4908f9b3000864d72ee98df2e7c28689c4342585e10bf0ac4cdf0d")
    XCTAssertTrue(key.publicKey.isValidSignature(sig, for: msg))
    XCTAssertTrue(key.publicKey.isValidSignature(try key.signature(for: msg), for: msg))
}
```

(`Data(hex:)` is a small helper you write yourself.)

### Frames

| Message | Frame bytes |
|---------|-------------|
| `ClientMessage{id: 1, ping: {}}` | `00000004` `08016200` |
| `ClientMessage{id: 0, terminal_input: {agent_id: "a1b2c3", data: "ls\r"}}` | `00000010` `ca020d0a0661316232633312036c730d` |

The first 4 bytes are the big-endian length. In the second frame, `ca02`
is the tag of field 41 (`terminal_input`) and `id` 0 is not encoded at all.

## 18. Client checklist

- [ ] Browse `_fleet._tcp`, require TXT `v=1`, key servers by TXT `id`.
- [ ] TLS 1.3, no chain validation, pin SHA-256 of the leaf DER; keep the
      observed `fp` per connection.
- [ ] Check `ServerHello.protocol_version == 1` and `server_id == hex(fp)`.
- [ ] Pairing: normalize the code locally, have the user confirm the
      fingerprint before sending the proof, verify `server_proof` before
      storing the pin.
- [ ] Device key in the Keychain (this device only); servers stored with
      server id, device id, name, last address.
- [ ] Non-zero request ids; `TerminalInput`/`TerminalResize` with id 0 and
      no reply expected.
- [ ] Separate connections for control/events and for each terminal.
- [ ] Read continuously; reconnect and resubscribe when dropped.
- [ ] Unknown device on auth: treat as unpaired. Certificate changed: show
      an explicit error, never re-pin silently.
- [ ] Unit tests against the vectors in section 17.
