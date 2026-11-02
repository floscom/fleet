# Teams: sharing skills with other people's fleets

Status: `internal/team` (crypto and protocol) and `fleet hub` (the server)
are built and tested; the gateway (daemon side, `fleet team ...`), the
skill-store integration and the dashboard are not yet.

A fleet is one person's machines, joined by the fleet key, and the fleet key
is full trust: peers run agents on each other, read files and unlock admin.
A team is a different trust level. It joins several people's fleets only
for the things each member chooses to publish, skills first and other kinds
later. No host, session, path, hostname or fleet key ever leaves a fleet.

## Goals

- A team member's fleet can be on any network: every daemon only connects
  out, to a hub.
- Multi-team: one fleet can be in several teams, on several hubs. Anyone
  can host a hub (`fleet hub`); codeline hosts one.
- End-to-end encrypted from the start: the hub stores and forwards
  ciphertext and cannot read skill names, skill contents or member names.
- Owned: a published skill belongs to its publisher; only the publisher
  can change or delete it. Everyone else subscribes to it.
- Nothing is shared by default: a skill reaches a team only through an
  explicit publish.

## Not in v1

- Live traffic between team members (shared sessions, running agents on a
  teammate's machine). The hub stores bundles and never relays requests to a
  daemon.
- Kinds other than skills. Bundles carry a `kind` inside their encrypted
  metadata, so agent definitions, rules, workflows and MCP configs (with
  secrets removed) can follow without hub changes.
- Editing someone else's skill. To change it, fork it (it becomes your
  own skill).

## Parts

```
 your fleet                      hub (fleet hub)                 teammate's fleet
 ┌──────────────┐  HTTPS, out    ┌──────────────────┐   HTTPS, out  ┌──────────────┐
 │ gateway      │ ─────────────► │ teams, rosters,  │ ◄──────────── │ gateway      │
 │ daemon       │                │ sealed keys,     │               │ daemon       │
 │  ↕ fleet key │                │ opaque bundles,  │               │  ↕ fleet key │
 │ other daemons│                │ event log        │               │ other daemons│
 └──────────────┘                └──────────────────┘               └──────────────┘
```

- **Hub**: `fleet hub --data DIR [--listen ADDR | --autocert DOMAIN]`, a
  separate mode of the same binary (`internal/hub`). It holds teams, signed
  rosters, sealed team keys, sealed bundles and a per-team event log. Its
  data lives in plain files: `teams/<id>/team.json` (the index, written
  atomically) and `teams/<id>/data/<ref>` (one file per bundle data, named
  by its hash), under a 0700 directory, `~/.fleet-hub` by default.
- **Gateway daemon**: the daemon a team was joined on holds that team's
  member key and is the only one that talks to the hub. The skills it
  installs from the team go into its skill store (`internal/skills`), and
  the existing fleet sync spreads them to the fleet's other daemons. It can
  publish any skill in the fleet's store, because the store is the same on
  every daemon.
- **Package `internal/team`**: keys, sealing, rosters and verification,
  invites, bundle heads, the hub API types and the hub client. No daemon
  logic, no storage. Its package doc is the reference for the gateway;
  `vectors_test.go` pins the canonical encodings.

## Identities and keys

All standard library: `crypto/ed25519`, `crypto/hpke` with
`MLKEM768X25519`, `crypto/hkdf`, plus `golang.org/x/crypto/chacha20poly1305`.

- **Member key**, one per (fleet, team), so two teams cannot link one
  fleet's memberships (`team.MemberKey`, saved as JSON with both seeds):
  - a signing key: Ed25519;
  - a key-wrapping key: an HPKE key pair, KEM `MLKEM768-X25519` (hybrid,
    post-quantum), KDF HKDF-SHA256, AEAD ChaCha20-Poly1305;
  - the member id is the first 16 bytes of SHA-256(signing public key),
    as hex.
- **Team key** `K[e]`: 32 random bytes per epoch `e`. Each member's copy is
  sealed to them with HPKE, info `canon("fleet-team-key-v1", team, e,
  member id)`. The roster commits to the key with `key_check =
  HMAC-SHA256(K[e], canon("fleet-team-key-check-v1", team, e))`: a member
  opening a sealed key checks it against the signed roster, so a hub cannot
  seal a key of its own to a member (and then read what that member
  publishes next). A new epoch starts whenever someone leaves or is
  removed, or an admin rotates.
- **Content key** `C`: 32 random bytes per bundle version. It seals the
  bundle's metadata and its tar.gz, and is itself sealed with `K[e]` (AAD
  `canon("fleet-team-wrap-v1", team, bundle, version, e)`). So a new epoch
  re-wraps a few small keys, and no content is uploaded again.
- **Symmetric sealing** is XChaCha20-Poly1305 with a random 24-byte nonce
  (`nonce || ciphertext`). Random nonces are safe at 192 bits without any
  counter state, which matters because the team key is used on many
  machines that share none.
- **Canonical encoding** (`canon`): everything signed, hashed or used as
  AAD/info is a domain label followed by length-prefixed fields (uint32
  big-endian length, bytes; uint64 big-endian numbers; one-byte booleans;
  lists as a count then items). Never JSON. Labels:
  `fleet-team-roster-v1`, `fleet-team-head-v1`, `fleet-team-key-v1`,
  `fleet-team-key-check-v1`, `fleet-team-wrap-v1`, `fleet-team-meta-v1`,
  `fleet-team-data-v1`, `fleet-team-name-v1`, `fleet-team-team-name-v1`,
  `fleet-team-invite-auth-v1`, `fleet-team-invite-proof-v1`,
  `fleet-team-invite-name-v1`, `fleet-team-member-v1`.

## Roster

The roster is the team's member list, signed by an admin:

```
roster {
  team, seq,                        // seq only goes up
  members: [{ id, sign_pub, kem_pub, admin, name_sealed }],  // sorted by id
  epoch, key_check,                 // current team key epoch and its commitment
  team_name_sealed,                 // the team's name, sealed with K[epoch]
  prev_hash,                        // SHA-256 of the previous roster's signed bytes
  signer, sig                       // an admin's id and Ed25519 signature
}
```

- `name_sealed` is the member's display name ("flo"), sealed with the team
  key (AAD binds team and member id), so the hub sees member ids but not
  names. When the epoch changes, the admin re-seals every name with the new
  key.
- **Chain rules** (`team.VerifyNext`): `seq = prev.seq + 1`; `prev_hash`
  matches; the signer is an admin *of the previous roster*; the signature
  verifies with that key; the epoch never falls and must rise when a member
  is dropped; continuing members keep their keys; `key_check` only changes
  with the epoch. A client keeps the highest `seq` it has seen and takes
  nothing lower (`team.Advance`).
- **Trust anchor.** A member does not trust history before it joined: a
  malicious hub could fabricate a whole chain from seq 1 with its own
  founder key, add the real admin's public key later, and nothing in the
  chain alone would show it. So:
  - the founder's anchor is its own seq 1 (`VerifyFounding`: one member,
    admin, self-signed);
  - a joiner's anchor is the first roster that lists the joiner, and it
    must be signed by the inviting admin whose member id is in the invite
    (`team.Anchor`). The id is the hash of the signing key the roster
    gives for that admin, so the hub cannot substitute one;
  - from the anchor on, the chain rules apply.
- A team can have several admins (`SetAdmin`); the last one cannot be
  demoted, and the hub refuses to let the last admin leave. The dashboard
  will suggest a second admin, so a team survives the loss of its founder's
  machine.

## Bundles

```
head {
  team, id,                 // id: random, stable for the life of the skill
  owner, version,           // version: Unix ms, strictly rising per bundle
  epoch, key_wrap,          // C sealed with K[epoch]
  meta_sealed,              // {kind:"skill", name, description, hash, size, files}, sealed with C
  data_ref, data_size,      // the tar.gz sealed with C: its hex SHA-256 (also its name on the hub) and size
  deleted,                  // tombstone: everything above but version is empty
  sig                       // owner's Ed25519 over the canonical encoding
}
```

- Only the owner publishes a version: the hub takes a head only from a
  request signed by `owner`, only with a rising `version`, and only once
  the data it references has been uploaded. Clients check the signature
  against their trusted roster (`team.VerifyHead`) and the data against
  the head (`team.VerifyData`), so a misbehaving hub can at most withhold
  bundles.
- **Re-wrap after an epoch change** is a new version of the same head with
  the same `meta_sealed` and `data_ref` and a new `key_wrap` under the new
  epoch's key (`team.Rewrap`): no upload. Metadata and data are sealed
  with AAD `canon(label, team, bundle)`, without the version, for this
  reason; the signed head binds them to the version.
- `meta_sealed` holds the name. The hub sees bundle ids, owners, sizes and
  times, but not what a skill is called or does.
- Limits: 16 MiB per skill, as today, plus a per-team quota set by the hub
  operator (`--quota`, 1 GiB by default).

## What the hub learns

It learns team ids, member ids and public keys, which member owns which
bundle, bundle sizes and change times, invite ids (hashes) and their
expiry, and the IP addresses of gateway daemons. It does not learn skill
names or contents, member or team names, hosts, sessions, paths or
anything else about a fleet. The hub's tests check that none of the
plaintext names or contents appear in its files.

## Flows

**Hub setup.** `fleet hub` starts with team creation closed. On start it
opens a loopback listener for the operator and writes `operator.json`
(token and address, 0600) into its data directory; `fleet hub code` reads
that file and asks the hub for a one-time team creation code (12 Crockford
base32 characters, one hour, one team). `--open` lets anyone create
teams. The hub never logs codes, credentials or tokens.

**Create a team.** `fleet team create https://hub.example.com codeline
--code ABCD-...` makes a member key, the first team key and roster `seq 1`
with the creator as admin (`team.Found`), and registers them on the hub
with the founder's own sealed key (`team.Create`). The team id is chosen by
the creator (random, 32 hex); the hub refuses a taken one.

**Invite.** `fleet team invite` (or a button in the dashboard) creates a
random 32-byte invite secret `I` and derives three keys from it with
HKDF-SHA256 (no salt, info `canon(label, team)`):

- the *credential* (`fleet-team-invite-auth-v1`): the joiner's one-time
  hub login; the hub stores only `SHA-256(credential)` as the invite's id,
  with an expiry (7 days by default, at most 30), single use;
- the *proof key* (`fleet-team-invite-proof-v1`): the joiner MACs its
  public keys with it, so the hub cannot substitute keys of its own;
- the *name key* (`fleet-team-invite-name-v1`): seals the joiner's display
  name on its way to the admin, because the joiner has no team key yet.

The admin registers the invite id and prints

```
fleet-team:https://hub.example.com/<team id>#<base64url(I)>.<inviting admin's member id>
```

to send to the person directly (chat, mail), never through the hub. The
admin's gateway keeps `I` until the invite is used or expires.

**Join.** `fleet team join <invite>` makes a member key and posts a join
request: the public keys, `HMAC-SHA256(proof key, canon("fleet-team-
invite-proof-v1", team, sign_pub, kem_pub))` and the sealed name, with the
credential as the hub login (`team.Join`). The hub marks the invite used,
records the request with the member id the keys give, and tells the
admins (event `join`). The inviting admin's gateway, long-polling the
team's events, verifies the proof and opens the name (`Invite.VerifyJoin`),
adds the member to a new roster with the name re-sealed under the team key
(`team.AddMember`), seals `K[e]` for them (`team.SealTeamKey`) and posts
both in one request; the hub drops the join request once a roster lists
the member. No click is needed; the invite is the approval. Until an admin
daemon has been online, the joiner shows "waiting for an admin's fleet".
The joiner then fetches the chain, anchors at the roster that lists it
(signed by the inviting admin), opens its sealed key and checks it against
`key_check`. Because the team key never travels in the invite, a leaked
invite is harmless once it has been used or has expired.

**Leave / remove.** An admin removes a member (`team.RemoveMember`): the
roster gets a new `seq` without them and the epoch goes up, with every
name re-sealed; `K[e+1]` is sealed for each remaining member and posted
with the roster. The hub revokes the removed member at once and drops
their bundles and uploads. A member leaving on their own (`POST leave`)
is revoked by the hub immediately, their bundles dropped, and they are
listed for the admins (`left` in the join list, event `left`) until a
roster without them is posted. Each owner's gateway, on seeing the new
epoch, re-wraps its current bundles. The removed member keeps what they
had already downloaded (no system can take that back) and gets nothing
new.

**Publish.** `fleet team publish git-commit [--team codeline]`, or a
toggle on the skill in the dashboard. The gateway reads the skill from the
store (`skills.Store.Archive`), seals it (`team.SealBundle`), uploads the
data (`PUT data/{ref}`) and then the head. After that, every local change
to the skill becomes a new version on the hub, sent once the folder has
held still, like the fleet sync; the hub frees the superseded data at
once. `fleet team unpublish` uploads a tombstone (`team.Tombstone`).

**Subscribe.** `fleet team skills` lists what members published (name,
owner, description, size, last change). `fleet team subscribe <owner>/<name>`
(or `<owner>/*` for everything one member publishes) installs a skill into
the skill store, and from there the fleet sync spreads it. Defaults:

- updates install automatically; `fleet team pin <owner>/<name>` keeps the
  current version, and the dashboard shows the diff of a pending update;
- the dashboard always shows who published a skill, because a skill's
  scripts run with the agent's rights;
- a bundle whose signature does not verify against the roster is refused
  and logged; a bundle whose owner has left the roster is dropped.

## Installing into the skill store

- `skills.Entry` gets a `Source` (team id, bundle id, owner id, bundle
  version). It syncs across the fleet with the rest of the manifest, so
  every daemon knows the skill came from a team, and only the gateway
  updates it.
- Names: a team skill installs under its own name unless that name is
  taken (by your own skill or by another member's), in which case it
  becomes `<owner>-<name>`. The installer then also rewrites the `name:`
  in SKILL.md's front matter, because the CLIs go by that name.
- A local edit to a subscribed skill forks it. The store sees a hash that
  is not the installed one, so the skill drops its `Source`, becomes your
  own, stops updating and shows "forked from flo/git-commit".
- A skill you published and also have locally is the same skill; it is
  never installed a second time.

## Hub API (JSON over HTTPS)

Every request is signed by a member key:

```
Authorization: Fleet-Member <member id>.<nonce>.<hex Ed25519 signature>
```

with the nonce from `GET /v1/nonce` (single use, 30 s) and the signature
over `"fleet-team-member-v1" "\n" team id "\n" nonce "\n" method "\n"
request URI "\n" hex(SHA-256(body))`, as the fleet-key MAC in
`web/peers.go`. The hub looks the member up in the team's current roster;
a revoked member gets 403, as does a stranger (an unknown team looks the
same). Two requests are not member-signed: team creation carries
`Authorization: Fleet-Create <code>` (none on an open hub) and a join
request `Authorization: Fleet-Invite <base64url credential>`. Those two
and the nonce are rate limited per IP (60/minute; `--trust-proxy` reads
`X-Forwarded-For`).

```
GET    /v1/nonce                                → {nonce}
POST   /v1/teams                 Fleet-Create   {roster, keys}                      201
GET    /v1/teams/{t}/roster?since=N   member    → {rosters: [seq > N, in order]}
POST   /v1/teams/{t}/roster      admin          {roster, keys}  next roster + sealed keys   204
GET    /v1/teams/{t}/keys        member         → {keys}  own sealed keys, current epoch
POST   /v1/teams/{t}/keys        admin          {keys}  sealed keys for members     204
POST   /v1/teams/{t}/invites     admin          {id, expires}                       201
POST   /v1/teams/{t}/joins       Fleet-Invite   {sign_pub, kem_pub, proof, name_sealed}   202
GET    /v1/teams/{t}/joins       admin          → {joins: [...], left: [member ids]}
DELETE /v1/teams/{t}/joins/{id}  admin                                              204
POST   /v1/teams/{t}/leave       member         revoked at once                     204
GET    /v1/teams/{t}/bundles     member         → {heads}
GET    /v1/teams/{t}/bundles/{b} member         → head
PUT    /v1/teams/{t}/bundles/{b} owner          head (version must rise)            204
PUT    /v1/teams/{t}/data/{ref}  member         raw sealed bytes; ref = hex SHA-256 204
GET    /v1/teams/{t}/data/{ref}  member         raw bytes, only data a live head references
GET    /v1/teams/{t}/events?since=N   member    long poll ≤ 25 s → {events, next, reset}
```

Errors are `{"error": "..."}` with the status: 401 (no or bad signature,
nonce, code), 403 (not a member, not an admin, not the owner), 409 (stale
roster seq, version not rising, team id taken, member joining again),
413 (body too large), 422 (fails verification), 429 (rate limit, too many
invites or joins), 507 (team quota used up).

Events are `{seq, time, kind, ref, member}` with kinds `roster` (ref:
seq), `keys` (member: who got a key; seen by them and admins), `join`
(ref: joiner id; admins), `left` (ref: member id; admins), `bundle` (ref:
bundle id). The log keeps the last 2000; a cursor older than that gets
`reset: true`, and the client fetches rosters, keys and heads afresh.

Hub-side rules, as a second line behind the clients': roster posts must be
signed by the requesting admin and pass `team.VerifyNext` against the
current head; heads must pass `team.VerifyHead` against the current
roster, come from their owner and reference uploaded data of the stated
size; sealed keys must be for the current epoch and for members; data
uploads must match their ref and fit the quota; an upload no head
references within an hour is deleted; a superseded or tombstoned
version's data is deleted at once; a roster that drops a member revokes
them and deletes their bundles.

Limits: JSON bodies 2 MiB, data 16 MiB + 1 KiB, 256 members, 64 open
invites and 64 pending joins per team, invite expiry ≤ 30 days, 4096
outstanding nonces.

## Test vectors

`internal/team/vectors_test.go` pins the deterministic parts of the
protocol against fixed seeds: member ids, the signed byte strings and
signatures of a roster and a head, `key_check`, the HPKE info and AADs,
the invite string and its derived credential, id and proof, and the
request signature message. A change to any label or encoding fails it and
must be made on purpose, with this document.

## Phases

1. `internal/team`: keys, HPKE sealing, content sealing, roster chain and
   bundle signatures, invites, API types and client, with test vectors.
   Done.
2. `fleet hub`: storage, the API above, quotas, creation codes, and long-poll
   events. Done.
3. Gateway: `fleet team create|invite|join|ls|members|remove|leave`, the
   event loop, admin auto-approval of joins, epoch rotation.
4. Skills: publish and subscribe, `Source` in the manifest, names, forks,
   pins.
5. Dashboard: a Team part in the skills view (publish toggles, team skills,
   subscribe, pending updates with diff, members, invite).
6. Later: other kinds; member keys shared across one's own fleet (sealed
   with the fleet key), so any daemon can act as the gateway.
