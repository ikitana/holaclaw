# Codex subscription flow — upstream assumptions

This package talks to an **unofficial** flow. Everything below was implemented
from the design notes and is covered only by recorded fixtures
(`contract_test.go`). Each item must be checked against the real service before
the provider is declared supported; all of them live in `auth.go`
(`deviceForm`, `exchangeForm`, `refreshForm`) or `provider.go`.

1. Device endpoints take JSON bodies. `/oauth/token` takes form-urlencoded bodies.
2. Refresh: `grant_type=refresh_token, client_id, refresh_token, scope="openid profile email"`.
3. Exchange: `grant_type=authorization_code, client_id, code, redirect_uri=https://auth.openai.com/deviceauth/callback`, with the poll response's `code_verifier`.
4. Device start: JSON `client_id`. Device poll: JSON `device_auth_id, user_code`. HTTP 403 and 404 from polling mean authorization is still pending.
5. Usercode response fields include `device_auth_id, user_code, interval`; interval may be a string or a number. A completed poll returns an authorization code and PKCE verifier (direct-token answers are handled too).
6. Error codes `authorization_pending | slow_down | expired_token | access_denied`; `invalid_grant | invalid_request | unauthorized_client | refresh_token_reuse_detected` mean logged out (`invalid_request` may be too aggressive).
7. `slow_down` widens the poll interval by 5 s.
8. Account id claim: `https://api.openai.com/auth → chatgpt_account_id`; expiry from `exp`.
9. Model list at `<backend>/codex/models`. The pinned fallback ids come from OpenAI's Codex models page; `gpt-6.1-sol` was confirmed against the live service on 2026-10-16.
10. `originator` is sent on auth requests too; header names `chatgpt-account-id`, `originator`, `session_id`.
11. An account with device code sign-in turned off (ChatGPT Security Settings) gets the user code all the same; OpenAI's page then refuses it. Not confirmed against the live service: polling is assumed to stay pending until the code expires, so `login.go` names the setting on a pending login and on an expired code.

When upstream drifts: update the fixtures in `testdata/`, then the form
builders, and nothing else.
