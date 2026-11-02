# Teams: sharing skills with other people's fleets

Status: design, not built.

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
 │ daemon       │                │ key wraps,       │               │ daemon       │
 │  ↕ fleet key │                │ opaque bundles,  │               │  ↕ fleet key │
 │ other daemons│                │ event log        │               │ other daemons│
 └──────────────┘                └──────────────────┘               └──────────────┘
```

- **Hub**: `fleet hub --data DIR --listen ADDR`, a separate mode of the same
  binary. It holds teams, signed rosters, wrapped team keys, encrypted
  bundles and a per-team event log. It runs behind a TLS proxy or with
  `--autocert DOMAIN`. Its data lives in plain files (JSON index plus one
  file per bundle version), the same as the rest of fleet.
- **Gateway daemon**: the daemon a team was joined on holds that team's
  member keys and is the only one that talks to the hub. The skills it
  installs from the team go into its skill store (`internal/skills`), and
  the existing fleet sync spreads them to the fleet's other daemons. It can
  publish any skill in the fleet's store, because the store is the same on
  every daemon.
- **Package `internal/team`**: keys, sealing, rosters and verification,
  with no networking. It has test vectors, like `identity/vectors_test.go`.

## Identities and keys

All of it uses the standard library: `crypto/ed25519`, `crypto/hpke`,
`crypto/mlkem`/`crypto/ecdh` and `golang.org/x/crypto/chacha20poly1305`
(already an indirect dependency; or `crypto/cipher` AES-256-GCM).

- **Member keys**, one set per (fleet, team), so two teams cannot link one
  fleet's memberships:
  - a signing key: ed25519;
  - a key-wrapping key: an HPKE key pair, KEM `MLKEM768X25519` (hybrid,
    post-quantum), KDF HKDF-SHA256, AEAD ChaCha20-Poly1305.
  - The member id is the first 16 bytes of SHA-256(signing public key),
    as hex.
- **Team key** `K[e]`: 32 random bytes per epoch `e`. Each member's copy is
  sealed to them with HPKE, info `"fleet-team-key-v1" | team id | e`.
  A new epoch starts whenever someone leaves or is removed.
- **Content key** `C`: 32 random bytes per bundle version. It encrypts the
  bundle's metadata and its tar.gz, and is wrapped with `K[e]`
  (AEAD, associated data `team id | bundle id | version | e`). So a new
  epoch re-wraps a few small keys, and no content is uploaded again.

## Roster

The roster is the team's member list, signed by an admin:

```
roster {
  team, seq,                       // seq only goes up
  members: [{ id, sign_pub, kem_pub, admin, name_sealed }],
  epoch,                           // current team key epoch
  prev_hash,                       // SHA-256 of the previous roster
  signer, sig                      // an admin's id and ed25519 signature
}
```

- `name_sealed` is the member's display name ("flo"), sealed with the team
  key, so the hub sees member ids but not names.
- The trust anchor is the founding admin's signing key, which a member
  learns from the invite (below). Every later roster must be signed by an
  admin of the previous one and chain to it through `prev_hash`. A client
  keeps the highest `seq` it has seen and rejects anything lower (no
  rollback), and it never trusts a public key it did not get from a
  verified roster. This stops the hub from slipping its own key into a
  team.
- A team can have several admins. The dashboard suggests a second one, so
  a team survives the loss of its founder's machine.

## Bundles

```
bundle version {
  team, id,                 // id: random, stable for the life of the skill
  owner, version,           // version: Unix ms, rises per bundle
  epoch, key_wrap,          // C wrapped with K[epoch]
  meta_sealed,              // {kind:"skill", name, description, hash, size, files}
  data_ref, data_hash,      // ciphertext of the tar.gz, stored separately
  deleted,                  // tombstone, as in skills.Entry
  sig                       // owner's ed25519 over all of the above
}
```

- The hub accepts a version only from its owner (a request signed by the
  owner's key, owner taken from the current roster) and only with a
  higher `version`. Clients check the signature against the roster too,
  so a hub that misbehaves can at most withhold bundles; it cannot change
  them.
- `meta_sealed` holds the name. The hub sees bundle ids, owners, sizes and
  times, but not what a skill is called or does.
- Limits: 16 MiB per skill, as today, plus a per-team quota set by the hub
  operator.

## What the hub learns

It learns team ids, member ids and public keys, which member owns which
bundle, bundle sizes and change times, and the IP addresses of gateway
daemons. It does not learn skill names or contents, member names, hosts,
sessions, paths or anything else about a fleet.

## Flows

**Hub setup.** `fleet hub` starts with team creation closed. The operator
runs `fleet hub code` to get a one-time team creation code; `--open`
lets anyone create teams.

**Create a team.** `fleet team create https://hub.example.com codeline
--code ABCD-...` makes member keys, the first team key and roster `seq 1`
with the creator as admin, and registers them on the hub.

**Invite.** `fleet team invite` (or a button in the dashboard) creates a
random 32-byte invite secret `I`, stores `SHA-256(auth(I))` on the hub with
an expiry (default 7 days, single use), and prints

```
fleet-team:https://hub.example.com/<team id>#<I>.<admin signing key fingerprint>
```

to send to the person directly (chat, mail), never through the hub.

**Join.** `fleet team join <invite>` makes member keys and uploads a join
request: the public keys plus `HMAC(I, team | sign_pub | kem_pub)`, with
`auth(I)` as the hub credential. The hub never sees `I` itself, so it can
neither forge a join request nor read the proof. The admin's gateway
daemon is subscribed to the team's events. It checks the proof, and if it
holds, it adds the member to a new roster, seals `K[e]` for them, and burns
the invite. No click is needed; the invite is the approval. Until an admin
daemon has been online, the joiner shows "waiting for an admin's fleet".
Because the team key never travels in the invite, a leaked invite is
harmless once it has been used or has expired.

**Leave / remove.** An admin removes a member (or the member leaves): the
roster gets a new `seq` without them and the epoch goes up. `K[e+1]` is
sealed for each remaining member, and the hub revokes the member's
credential. Each owner's gateway, on seeing the new epoch, re-wraps the
content key of its current bundles. The removed member keeps what they had
already downloaded (no system can take that back) and gets nothing new.

**Publish.** `fleet team publish git-commit [--team codeline]`, or a
toggle on the skill in the dashboard. The gateway reads the skill from the
store (`skills.Store.Archive`), seals it and uploads it. After that, every
local change to the skill becomes a new version on the hub, sent once the
folder has held still, like the fleet sync. `fleet team unpublish` uploads a
tombstone.

**Subscribe.** `fleet team skills` lists what members published (name,
owner, description, size, last change). `fleet team subscribe <owner>/<name>`
(or `<owner>/*` for everything one member publishes) installs a skill into
the skill store, and from there the fleet sync spreads it. Defaults:

- updates install automatically; `fleet team pin <owner>/<name>` keeps the
  current version, and the dashboard shows the diff of a pending update;
- the dashboard always shows who published a skill, because a skill's
  scripts run with the agent's rights;
- a bundle whose signature does not verify against the roster is refused
  and logged.

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

Every request is signed by a member key (`Authorization: Fleet-Member
<member id>.<nonce>.<sig>`, nonce from `GET /v1/nonce`, signed over the same
fields as the fleet-key MAC in `web/peers.go`), except team creation (code)
and join (invite credential).

```
POST /v1/teams                              create (creation code)
GET  /v1/teams/{t}/roster                   current roster + chain since seq
POST /v1/teams/{t}/roster                   new roster (admin)
GET  /v1/teams/{t}/keys                     my sealed team keys
POST /v1/teams/{t}/keys                     sealed keys for members (admin)
POST /v1/teams/{t}/invites                  register invite hash (admin)
POST /v1/teams/{t}/joins                    join request (invite credential)
GET  /v1/teams/{t}/bundles                  all bundle heads
GET  /v1/teams/{t}/bundles/{b}              head + data_ref
GET  /v1/teams/{t}/data/{ref}               ciphertext
PUT  /v1/teams/{t}/bundles/{b}              new version (owner)
POST /v1/teams/{t}/bundles/{b}/wraps        re-wrapped keys after an epoch (owner)
GET  /v1/teams/{t}/events?since=N           long poll: roster, keys, joins, bundles
```

## Phases

1. `internal/team`: keys, HPKE sealing, content sealing, roster chain and
   bundle signatures, with test vectors.
2. `fleet hub`: storage, the API above, quotas, creation codes, and long-poll
   events.
3. Gateway: `fleet team create|invite|join|ls|members|remove|leave`, the
   event loop, admin auto-approval of joins, epoch rotation.
4. Skills: publish and subscribe, `Source` in the manifest, names, forks,
   pins.
5. Dashboard: a Team part in the skills view (publish toggles, team skills,
   subscribe, pending updates with diff, members, invite).
6. Later: other kinds; member keys shared across one's own fleet (sealed
   with the fleet key), so any daemon can act as the gateway.
