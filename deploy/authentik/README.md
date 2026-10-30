# Authentik setup for tasmofleet

Example deployment. tasmofleet is a confidential OIDC client
(authorization code flow with PKCE); roles come from the `groups` claim.

## Groups

| Group               | tasmofleet role | Env                                  |
| ------------------- | --------------- | ------------------------------------ |
| `Tasmofleet Admins` | admin           | `ADMIN_GROUPS=Tasmofleet Admins`     |
| `Tasmofleet Users`  | operator        | `OPERATOR_GROUPS=Tasmofleet Users`   |

A user in none of the groups gets a 403 page and no session.

## OAuth2/OpenID provider

| Setting             | Value                                                                    |
| ------------------- | ------------------------------------------------------------------------ |
| Client type         | Confidential                                                             |
| Redirect URI        | `https://tasmofleet.example.com/oidc/callback` (strict match)          |
| Signing key         | an RSA certificate, so ID tokens are RS256                               |
| Scopes              | `openid`, `profile`, `email` (`profile` carries `groups` in Authentik)   |
| Subject mode        | based on the user's hashed ID (stable `sub`)                             |
| Issuer              | per provider: `https://auth.example.com/application/o/tasmofleet/`     |
| Grant types         | `authorization_code` and `refresh_token`                                 |

tasmofleet also requests `offline_access` to get a refresh token. If the provider does not
issue one, sessions are capped to the re-check interval (`OIDC_RECHECK`, 15 minutes) and the
start log says so; select the `offline_access` scope mapping on the provider.

Providers created through the API need `grant_types` set explicitly to
`authorization_code` and `refresh_token`; the UI does this for you.

Create an application `tasmofleet` (slug `tasmofleet`, so the issuer above matches),
bind the two groups to it, and restrict access through those bindings.

## tasmofleet side

Set `PUBLIC_URL` (container configuration), then fill in the rest under
System settings, "Single sign-on" (admin, needs your password), and press
"Test discovery". The values are stored in the database; they take effect after a
restart, and local accounts keep working if anything is wrong. Alternatively set them
in the container configuration (the page then shows them read-only):

```
AUTH_MODE=oidc
PUBLIC_URL=https://tasmofleet.example.com
OIDC_ISSUER=https://auth.example.com/application/o/tasmofleet/
OIDC_CLIENT_ID=<from the provider>
OIDC_CLIENT_SECRET_FILE=/data/oidc-client-secret
ADMIN_GROUPS=Tasmofleet Admins
OPERATOR_GROUPS=Tasmofleet Users
```

Never put the client secret into a file in this repository.

## Header mode (alternative)

Use a proxy provider with an Authentik outpost behind Traefik forward auth
(see `deploy/traefik/tasmofleet.yml`), `AUTH_MODE=header`, and
`TRUSTED_PROXIES` set to the Traefik host. Headers are honoured only from that
peer address.

## Application icon

Set the application's icon (Admin -> Applications -> tasmofleet -> Icon) to
`assets/branding/icon-512.png` from this repository (upload the file, or use
`https://raw.githubusercontent.com/nofuturekid/tasmofleet/main/assets/branding/icon-512.png`).
