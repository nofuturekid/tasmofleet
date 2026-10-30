# Local accounts: lifecycle, authority and revocation

Scope: local accounts (`AUTH_MODE=local`, or `LOCAL_LOGIN=true` next to OIDC or
header mode), their sessions and device cookies, the admin web UI and the host
CLI. (The direct admin listener was removed on 2026-10-19, see
`admin-listener.md`.) Code: `internal/auth/{accounts,accountweb,local,session,service,stepup,bootstrap}.go`,
`internal/store/auth.go`, `cmd/tasmofleet/users.go`. Login rate limiting is
specified separately in `login-rate-limiting.md`.

Review findings are judged against this document. Every claim (L1 to L15) has
a test.

## Model

- **Account**: username (lower case), PBKDF2 hash, role (`operator` or `admin`,
  nothing else), `disabled`, `must_change_password`, `updated_at` (last password
  change) and **`password_gen`**.
- **Password generation** (`password_gen`): starts at 1, incremented **in the
  same SQL statement** by every password change (own, admin reset, CLI) and by
  disabling the account. Role changes and enabling do not touch it.
- **Session** (local): carries the generation its login **verified**. **Device
  cookie row**: carries the same.
- **Actor**: the signed-in admin on whose authority a web mutation runs
  (username + the generation of its session). The host CLI is a trusted caller
  without an actor.

## Invariants

| ID  | Invariant                                                                                                                                                                                                                                              |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| L1  | A local session is valid on **every request** only while its account exists, is enabled and `password_gen` equals the session's generation. Deleting session rows is cleanup, not the revocation mechanism.                                           |
| L2  | A local session or device cookie row is **stored** only if, in the same SQL statement, the account is enabled and still at the generation the login verified. A login that raced a password change or disable cannot create either.                   |
| L3  | The generation a login stores is the one read **before** the password hash was verified, never one read afterwards.                                                                                                                                  |
| L4  | A device cookie counts as "recognised" only with a valid MAC, a row, the current generation, an age under 180 days and an issue time not before the last password change.                                                                             |
| L5  | Roles are read from the account on every request; a role change applies at once without ending sessions. Only `operator` and `admin` exist; a zero or unknown role is refused by the store.                                                           |
| L6  | Every web mutation of an account (create, role, disable, enable, password reset, delete) is applied only if, **in the same SQL statement**, the actor is still an enabled admin at the generation of its session (`ErrStaleActor` otherwise).      |
| L7  | The owner's own password change is a compare-and-set on the generation its session was validated against and on `disabled = 0`. A concurrent reset or change by someone else wins; the stale one is refused (409).                                     |
| L8  | The last enabled admin cannot be demoted, disabled or deleted, from the web or the CLI, even by two writers at the same moment (one conditional statement). A disabled admin does not count.                                                          |
| L9  | `Add` is a plain `INSERT`: an existing account (role, password) is never overwritten, however many writers race.                                                                                                                                     |
| L10 | A password change or disable also deletes the user's sessions and device cookie rows. This is cleanup and immediate feedback; L1 and L2 make correctness independent of it.                                                                          |
| L11 | A step-up (current password re-entered, 5 min) is bound to the session id **and** its generation, lives in memory, and is dropped by the user's own password change. It does not carry over to another session or a later generation.                  |
| L12 | Only admins reach `/users`. Viewing needs no step-up; every change needs one (local password, or the IdP re-authentication for OIDC admins); header principals have none and cannot change anything. An OIDC admin acts without the per-actor check a local admin gets, so the last-admin guard (store) is what protects against lock-out. An operator reaches only its own account page (`GET /account`) and `POST /account/password` for its own local account; that form reads only `current`, `new`, `confirm`.        |
| L13 | A user with `must_change_password` reaches nothing but `/account` (the forced change), `POST /account/password` and `/logout` until the password is changed.                                                                                                                              |
| L14 | The generated initial admin password is never logged. It is written once to `<DATA_DIR>/initial-admin-password` (0600, `O_EXCL`) and removed after the first change (and at start when the account no longer needs it).                                |
| L15 | _Removed 2026-10-19 with the direct admin listener (sessions no longer record a listener)._ |

## Mechanism

**Login** (`localSubmit`): rate limit (see the other document) -> read the
account (this read fixes the generation, L3) -> verify the hash -> create the
session and the device cookie row **conditioned on that generation** (L2). If
the condition fails the answer is the generic 401 and the audit line
`denied: account changed during login`.

**Every request** (`revalidate`): re-read the account; reject on missing,
disabled, generation mismatch, a `updated_at` newer than the session (covers
out-of-band edits) or a role other than operator/admin (L1, L5).

**Web mutations**: the handler gets the actor from the validated session and
calls `Accounts.As(actor)`; the store appends the actor condition to the
statement (L6). Without `As` a call runs with CLI authority, so `acting()` is
the only constructor the handlers use.

**Own password change**: current password verified (rate limited in the
separate `pw:<user>` scope) -> `ChangeOwnPassword(expectGen)` (L7) -> all
sessions and cookie rows of the user deleted (L10) -> this browser gets a new
session and cookie bound to the new generation.

**Migration 0003** (admin listener, removed 2026-10-19; the column stays unused) adds `sessions.listener` (default `main`), so sessions that
exist at the upgrade stay on the main listener only.

**Migration 0002** gives existing break-glass accounts the role `admin`,
generation 1, and existing sessions generation 0, so sessions created before
the upgrade end at the upgrade.

## Attackers

| ID  | Attacker                                                              | Goal                                               |
| --- | --------------------------------------------------------------------- | -------------------------------------------------- |
| B1  | knows an old password, logs in while the victim changes it            | keep a session after the change                    |
| B2  | holds a stolen session or device cookie                               | keep using it after revocation                     |
| B3  | admin whose rights are being withdrawn, requests in flight            | finish one more privileged change                  |
| B4  | operator                                                              | create or raise accounts, reset someone's password |
| B5  | two honest writers (web admin, CLI, second container) at once         | zero admins, overwritten account                   |
| B6  | holder of the database file                                           | read refresh tokens, replay sessions               |

## Claims and tests

| Claim | Test                                                                                                                                                                      |
| ----- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| L1    | `TestStaleGenerationSessionIsRejectedOnEveryRequest`, `TestAdminManagesAccounts` (disable ends the running session)                                                       |
| L2    | `store.TestSessionAndDeviceCookieAreBoundToTheGeneration`, `TestLoginInFlightDuringPasswordChangeIsDead` (password change and disable)                                   |
| L3    | `TestLoginInFlightDuringPasswordChangeIsDead`                                                                                                                             |
| L4    | `TestDeviceCookieCannotBeForgedOrReused`, `TestDeviceCookiesAreRecordedReusedAndRevocableIndividually`                                                                   |
| L5    | `TestAdminManagesAccounts` (promotion applies to a running session), `store.TestSetLocalUserRejectsRolesOtherThanOperatorAndAdmin`, `TestLoginUsesTheAccountsRole`      |
| L6    | `store.TestActorMustStillBeAnEnabledAdminAtWriteTime`, `TestAdminActionIsRefusedWhenTheActorChangedMidRequest` (3 ruins x 5 actions)                                     |
| L7    | `store.TestChangeOwnPasswordIsCompareAndSet`, `TestOwnPasswordChangeLosesAgainstAConcurrentReset`                                                                        |
| L8    | `store.TestLastAdminRule`, `store.TestConcurrentAdminRemovalLeavesOneAdmin`, `TestLastAdminIsProtected`, `TestCLIProtectsTheLastAdmin`, `TestConcurrentWritersKeepAccountInvariants` |
| L9    | `store.TestAddLocalUserNeverOverwrites`, `TestConcurrentWritersKeepAccountInvariants`                                                                                    |
| L10   | `TestSetPasswordRevokesSessionsAndDeviceCookies`, `TestDisableRevokesAndEnableDoesNotResurrect`, `TestUserPasswdKeepsStateAndRevokesSessions`                            |
| L11   | `TestStepUpIsBoundToSessionAndGeneration`, `TestAdminStepUp`                                                                                                              |
| L12   | `TestOperatorCannotManageAccounts`, `TestEveryChangeNeedsStepUp`, `TestHeaderAdminIsViewOnly`, `TestOIDCAdminStepUpAllowsLocalAccountChanges`, `TestExternalAdminsHaveNoPasswordForm`, `TestOwnPasswordChange`, `TestOnlyAccountRoutesChangeCredentials`                     |
| L13   | `TestStandaloneFirstRun`, `TestStandaloneEndToEnd`                                                                                                                        |
| L14   | `TestStandaloneFirstRun`, `TestInitialPasswordFileRemovedAtStartupWhenSpent`                                                                                             |
| L15   | _removed_ |
| B6    | `store.TestSessionRefreshTokenIsSealedAtRest`, `store.TestMigration0002KeepsExistingData`                                                                                |

## Accepted residual risks

| ID  | Risk                                                                                                                                              | Why accepted                                                                                                                       |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| A1  | Step-up state is in memory: a restart makes admins re-enter the password.                                                                        | Fails closed; one prompt.                                                                                                          |
| A2  | A request that was already past `revalidate` when a password changed can still **read** a page; it cannot mutate accounts (L6) or start a new session (L2). | Reads are bounded by the request duration; no state change.                                                                        |
| A3  | The host CLI acts without an actor. Anyone with shell access to the data directory can also read the database.                                   | Host access is the trust boundary; the CLI is the break-glass path (`user passwd --create --role admin`).                                          |
| A4  | A session created by a login that verified generation N is valid until generation N+1 appears; there is no per-session "log out everywhere" besides the generation. | `tasmofleet sessions revoke --user` deletes the rows; password change or disable bumps the generation.                              |
| A5  | Disabling an account then enabling it does not restore old sessions (generation was bumped).                                                      | Intended.                                                                                                                          |
| A6  | One process-wide mutex serialises OIDC re-checks.                                                                                                 | Availability only, with a 10 s timeout.                                                                                            |
