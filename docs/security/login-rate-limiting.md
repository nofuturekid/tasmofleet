# Break-glass login rate limiting

Scope: `POST /login/local` (local break-glass admin, spec §15.1). OIDC and
trusted-header logins are rate limited by the identity provider or proxy,
not here. Code: `internal/auth/ratelimit.go`, `internal/auth/local.go`.

Account lifecycle, sessions and revocation are specified in
`account-lifecycle.md`.

Brute-force resistance and lockout resistance pull against each other. A
budget that stops guessing can also be spent by an attacker to keep the
admin out. This document fixes the trade-off. The implementation must
match it, and every claim (C1 to C14) has a test. Review findings are
judged against this document.

## Attacker models

| ID  | Attacker                                                    | Goal                            |
| --- | ----------------------------------------------------------- | ------------------------------- |
| A1  | single host (one IPv4 address or one IPv6 /64)              | guess the password              |
| A2  | single host rotating usernames                              | guess or enumerate accounts     |
| A3  | botnet (many IPv4 addresses) or IPv6 rotation over many /64 | guess the password in aggregate |
| A4  | any of the above                                            | keep the real admin out (DoS)   |
| A5  | flood of distinct networks and usernames                    | exhaust memory or CPU           |
| A6  | attacker holding a stolen device cookie, possibly a botnet  | skip the global tiers and guess |
| A7  | LAN client sending forged `X-Forwarded-For`                 | pose as many networks           |

## Mechanism

**Client network.** The client network is the IPv4 address or the IPv6 /64
of the client address. The client address is the TCP peer. Only when the
peer is in `TRUSTED_PROXIES` is it the rightmost untrusted
`X-Forwarded-For` hop instead (C1).

**Admission.** Every attempt is checked against the limiter and counted
when it is admitted. This happens before the account lookup and before
the password hash is computed, for existing and unknown usernames alike
(C2). Refused attempts are not counted. A refused attempt is audited as
`denied: rate limited` and answered with 429.

**Tiers for unrecognised clients.** All of them apply together:

| Tier                   | Budget        | After the budget                                          | Memory |
| ---------------------- | ------------- | --------------------------------------------------------- | ------ |
| (network, username)    | 5 attempts    | delay since last attempt: 1, 2, 4, 8 min, then 15 min max | 1 h    |
| network, all usernames | 20 per 15 min | 1 per minute                                              | 15 min |
| username, all networks | 30 per 15 min | 1 per 10 s                                                | 15 min |

- The pair delay never exceeds 15 min, so there is no permanent lock (C3).
- A successful login clears only its own (network, username) pair (C4).
- Rotating usernames from one network hits the network tier (C5).
- Distributed guessing (A3) is bounded to at most 30 + 90 = 120 attempts
  per 15 min per username (C6).

**Recognised clients.** A client is recognised if it presents a valid
device cookie for exactly that username (C7). Recognised clients skip the network and username
tiers. Instead they get:

- their own (network, username) table, with the same rules as above (C8);
- their own per-username tier: 10 per 15 min, then 1 per minute. This
  bounds a stolen cookie used from a botnet (A6) to at most 25 attempts
  per 15 min. Unrecognised clients cannot spend this budget (C9).

**Device cookie** (OWASP "device cookies"):

- It is issued on a successful local login, unless the browser already presents a valid one.
- Format: `v1.<b64url username>.<issued unix ms>.<random id>.<b64url HMAC-SHA256>`.
- The MAC covers everything before the last dot. It is computed with a key
  derived from the persisted `SecretKey` under its own purpose label, so it
  is separate from the CSRF key.
- The MAC is verified in constant time before any field is parsed.
- The cookie is rejected if it:
  - has a missing, empty or wrong MAC;
  - has an unknown version;
  - was issued for another username;
  - was issued more than 1 min in the future;
  - is older than 180 days;
  - was issued before the account's last password change (`LocalUser.UpdatedAt`)
    (C10).
- Every issued cookie is recorded (SHA-256 of its random id, `device_cookies`
  table, at most 50 per user, oldest evicted). A validly signed cookie
  without a row is rejected, so one cookie can be revoked alone
  (`Service.RevokeDeviceCookie`). A browser that already presents a valid
  cookie keeps it at login instead of getting another one.
- Each row (and each session) carries the account's password generation, which
  the store increments on every password change and disable. A cookie or
  session is valid only while its generation is current, so a login that
  verified the old password and finishes after the change is dead on arrival.
- Revocation: any password change or disable of that user (CLI or web) deletes
  all of the user's rows and sessions, and rotating the key file revokes all
  cookies.
- Cookie flags: HttpOnly, SameSite=Strict, and Secure plus the `__Host-`
  prefix when `PUBLIC_URL` is https.

**Memory and CPU bounds** (A5):

- Keys are 16-byte SHA-256 prefixes, so long usernames cost nothing.
  Usernames are also cut to 128 bytes before use.
- Each unrecognised table holds at most `MaxKeys` entries (default 100 000).
  Each recognised table holds at most 4 096 entries.
- New keys of a full table share one overflow bucket per tier. That bucket
  obeys the same limits, so it stays bounded and is never an open door.
- Expired entries are pruned at most once per minute (C11).
- At most 2 password-hash checks (PBKDF2, 600 000 iterations) run at a time.
  Waiting clients that give up free their place (C12).

**Unknown users.** An unknown username is checked against a dummy hash
with the same cost, so response time does not reveal which accounts exist
(C13). Spoofed `X-Forwarded-For` from an untrusted peer changes nothing
(C14).

## Accepted residual risks

| ID  | Risk                                                                                                                                                              | Why accepted / mitigation                                                                                                                                                                                                                                                |
| --- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| R1  | A botnet (A3/A4) can keep the username tier saturated. The admin logging in from a **new device** then competes for one attempt per 10 s. | Bounding A3 needs a shared budget, and any shared budget can be spent by an attacker. Mitigations: the device cookie from any earlier login, and the CLI on the host (`user passwd --create --role admin`, local access). OIDC remains the primary login. |
| R2  | A flood that fills a table (A5) puts new unrecognised clients into the shared overflow bucket, which an attacker can keep throttled.                              | Memory stays bounded. Recognised clients have separate tables. Tables recover once entries expire (at most 1 h). Mitigations as in R1.                                                                                                                                   |
| R3  | A stolen device cookie skips the global tiers. It is still limited per pair and by the recognised per-user tier (25 per 15 min).                                  | Stealing an HttpOnly cookie needs a compromised browser or host. Revoke with `user passwd` or by rotating `SecretKey`. With a password of at least 12 characters and PBKDF2 at 600 000 iterations, 100 guesses per hour are useless.                              |
| R4  | A thief of the device cookie can throttle the real admin's recognised tier.                                                                                       | The same compromise as R3. Revocation (password change) restores access.                                                                                                                                                                                                 |
| R5  | One client gets separate pair counters with and without a cookie, so a cookie holder gets 5 more free attempts.                                                   | Negligible against the password policy.                                                                                                                                                                                                                                  |
| R6  | Limiter state lives in memory, so a restart resets it.                                                                                                            | An attacker cannot trigger restarts through the login path. Restarts are rare.                                                                                                                                                                                           |
| R7  | A device cookie is revoked individually only through code (`RevokeDeviceCookie`); there is no UI listing a user's devices.                                       | Revocation per user (password change, disable) and for all cookies (key rotation) covers the realistic cases. A device list can be added on top of the `device_cookies` table.                                                                                           |
| R8  | **Wired** (`app.setupAuth` loads `csrf.key`; `TestSetupAuthPersistsKeyAndSeedsInitialAdmin`, standalone e2e restart). If `<DATA_DIR>/csrf.key` is deleted, device cookies and header-mode CSRF tokens end.                                                                              | The key is created once (0600, `auth.LoadOrCreateKey`) and never regenerated silently; a wrong size or loose mode stops the start. Cookies only skip the global tiers, so their loss costs convenience, not safety.                                                          |

## Claims and tests (`internal/auth`)

| Claim | Test                                                                                                                                                              |
| ----- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| C1    | `TestLimitKey`, `TestRateLimitGroupsIPv6By64`, `httpx.TestClientIPFromForwardedChain`                                                                             |
| C2    | `TestLimiterCountsAtAdmission`, `TestUnknownUserIsRateLimited`, `TestLocalLoginRateLimit` (attempt 6 refused, audited)                                            |
| C3    | `TestLocalLoginRateLimit` (1, 2, 4 min steps), `TestPairDelayCapped`                                                                                              |
| C4    | `TestSuccessClearsOnlyOwnPair`                                                                                                                                    |
| C5    | `TestUsernameRotationFromOneNetwork`, `TestNetworkTierSlowRate`                                                                                                   |
| C6    | `TestDistributedGuessingIsSlowed`, `TestLoginAppliesUserWideBudget`, `TestAttackerCannotLockOutBreakGlassAdmin`                                                   |
| C7    | `TestDeviceCookieBypassesGlobalTiers`                                                                                       |
| C8    | `TestDeviceCookieStillPairLimited`                                                                                                                                |
| C9    | `TestRecognisedUserTier`                                                                                                                                          |
| C10   | `TestDeviceCookieCannotBeForgedOrReused` (tampered, missing, empty or foreign MAC, swapped user, cross-user replay, other key, future, expired, revoked, version), `TestDeviceCookiesAreRecordedReusedAndRevocableIndividually` |
| C11   | `TestLimiterMemoryBounded` (1 000 000 distinct keys)                                                                                                              |
| C12   | `TestKDFConcurrencyBounded`                                                                                                                                       |
| C13   | `TestPasswordHash` (dummy hash costs a full KDF run)                                                                                                              |
| C14   | `TestSpoofedForwardedForDoesNotEvadeLimit`                                                                                                                        |
