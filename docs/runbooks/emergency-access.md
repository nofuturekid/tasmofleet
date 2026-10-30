# Emergency access: signing in while the IdP or the proxy is down

Use this when Authentik (or another IdP) does not work and you still have to
reach tasmofleet, for example to see the audit log, stop a running update or fix
an account.

**Break-glass is a local admin on the normal login page, plus the host CLI
`tasmofleet user passwd`.** There is nothing else: the direct admin listener
(`ADMIN_LISTEN`, port 8443) was removed on 2026-10-19 (see
`docs/security/admin-listener.md`, superseded). It is not Fort Knox: the only
protection on the login page is the login rate limiting
(`docs/security/login-rate-limiting.md`).

|     |                                                                                       |
| --- | ------------------------------------------------------------------------------------- |
| URL | the normal address of tasmofleet (public name, or `http(s)://<container IP>:<port>/`) |
| Who | a **local** account with the admin role (not an IdP account)                          |

## 0. Set up once (before you need it)

1. `LOCAL_LOGIN` stays `true` (the default).
2. Make sure a local admin exists and its password is in the password manager
   (section 2 creates one). Test the sign-in once while everything works.
3. If the public name is behind a forward-auth proxy that is itself down, use an
   address that does not go through it: the container and the `LISTEN` port,
   e.g. `http://<container>:8080/` (for example
   `http://tasmofleet.example.internal:8080/`). Plain HTTP works for the local
   sign-in even when `PUBLIC_URL` is `https://...`: cookies are Secure (and
   `__Host-` prefixed) only when the request itself is HTTPS (TLS listener, or
   `X-Forwarded-Proto: https` from a `TRUSTED_PROXIES` address). Single sign-on
   needs the public URL. With the zero-configuration self-signed certificate the
   browser warns once.

## 1. Sign in

1. Open the login page (`/login`). With OIDC configured it shows the button
   "Sign in with ..." and, below it, the username/password form. The browser does
   not jump to the IdP while local sign-in is on, so the form is always reachable.
2. Enter the local admin and its password. After a forced password change you are
   on the device list.
3. **Sign out** when done. The session also ends after 2 h idle or 12 h in total (the defaults of `SESSION_IDLE` and `SESSION_MAX`).

## 2. No local admin exists, or the password is lost

On the host (shell access is the trust boundary). The password goes through stdin
so it does not appear in the process list; flags come **before** the name.

```sh
# new admin
docker exec -i tasmofleet tasmofleet user add --role admin <name>
# reset a password (also revokes the account's sessions and trusted browsers)
docker exec -i tasmofleet tasmofleet user passwd <name>
# list accounts, revoke all sessions of one account
docker exec -i tasmofleet tasmofleet user list
docker exec -i tasmofleet tasmofleet sessions revoke --user <name>
```

Use at least 12 characters. Add `--must-change` to force a change at the first
sign-in. Every CLI action is audited with actor `cli`. The CLI is not rate limited.

## 3. Troubleshooting

| Symptom                                            | Cause and fix                                                                                                                                                                                                                              |
| -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| The login page shows no form                       | Local sign-in is off (`LOCAL_LOGIN=false`) or no local account exists. Create one with the CLI (section 2) and set `LOCAL_LOGIN=true`.                                                                                                     |
| The browser goes straight to the IdP               | Only when OIDC is the only method. Same cause and fix as above.                                                                                                                                                                            |
| "Too many attempts" (HTTP 429)                     | The login limiter. Wait the time shown (1 min after the 5th wrong attempt, then longer). A browser that signed in before (device cookie) is limited more gently than a new one. The host CLI (section 2) is not rate limited.                |
| "missing or invalid CSRF token" on the direct address | Before 0.11.1, an `https://` `PUBLIC_URL` made every cookie Secure, and the browser dropped them on plain HTTP. Update to 0.11.1 or later. |
| The old address `/login/local` or port 8443 fails  | `/login/local` redirects to `/login`; port 8443 was the removed admin listener. A set `TASMOFLEET_ADMIN_*` variable is ignored with a warning at start; remove it and the firewall rule. The old certificate directory `<DATA_DIR>/tls/admin/` can be deleted.                                                   |
