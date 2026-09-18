# Authentication

🇫🇷 [Lire ce document en français](AUTHENTICATION.fr.md)

## Background

As of April 2026, Qobuz added reCAPTCHA to their `user/login` API endpoint, breaking
direct email/password login for all third-party tools. Token-based authentication is
now the only supported method.

## How to get your credentials

1. Open [https://play.qobuz.com/login](https://play.qobuz.com/login) in a browser and log in with your Qobuz account.
2. Open DevTools (F12 or Ctrl+Shift+I / Cmd+Option+I on macOS).
3. Go to the **Network** tab.
4. Type `user/login` in the filter box.
5. Reload the page and wait for Qobuz to finish loading.
6. Click the `login` request that appears in the list.
7. Open the **Response** tab in the right-hand panel.
8. Copy two values from the JSON response:
   - `"id"` → this is your **user ID** (a numeric string)
   - `"user_auth_token"` → this is your **auth token** (a long alphanumeric string)

## config.yaml

```yaml
user_id: "12345678"
user_auth_token: "your-user-auth-token-here"
```

## Token lifetime

`user_auth_token` is a long-lived session token — it does not expire after a fixed time
like a JWT. It remains valid until you explicitly log out of Qobuz or revoke sessions
from your account settings. You should not need to refresh it frequently.

## Unauthenticated mode (experimental)

If you'd rather not put any Qobuz credentials in `config.yaml`, gobz-connect
can skip local authentication entirely:

```yaml
unauthenticated_mode: true
```

With this set, `user_id`/`user_auth_token` are not needed. Instead,
gobz-connect starts only its mDNS advertisement and WebSocket listener, and
waits for the Qobuz app to select it from the Connect device picker. When the
app connects, it hands the renderer its own short-lived credentials
(`jwt_api`) over mDNS (`connect-to-qconnect`) — the same handoff the app uses
to control any Connect device, not something specific to gobz-connect.

**The catch:** that handoff only happens when a user actively picks this
renderer in the app's UI. `jwt_api` expires after a while, and nothing in the
Qobuz app's protocol guarantees it will proactively renew it on its own —
only re-selecting the renderer does. In practice this means:

- Tracks already downloaded/queued keep playing normally even after the
  token expires.
- Loading a *new* track (one that wasn't already fetched) can fail once the
  token has expired, until the app reconnects — gobz-connect waits up to 45
  seconds for that to happen before giving up on the request and moving on.
- For a long, unattended listening session (e.g. background music playing
  for hours with nobody touching the Qobuz app), you may eventually see
  playback stop advancing through the queue until you reopen the app and
  reselect the renderer.

This is a limitation of the underlying Connect protocol, not something
gobz-connect can fully work around on its own. For unattended, long-running
use, prefer `user_id`/`user_auth_token` above, which self-renews and doesn't
depend on the app reconnecting. Reserve `unauthenticated_mode` for quick
testing when you don't want to set up credentials at all.
