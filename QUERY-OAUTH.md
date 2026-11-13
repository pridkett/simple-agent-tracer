# OAuth for model-call queries

Pocket ID can issue access tokens directly to ChatGPT and Claude Desktop for
the tracer's remote MCP endpoint. The tracer advertises Pocket ID to MCP
clients, and Pocket ID handles browser sign-in, authorization code with PKCE,
consent, and token refresh.

## Configure the tracer

Use Pocket ID 2.14 or newer for its RFC 9068 access tokens, authorization
server discovery, and Client ID Metadata Document support. Set
`TRACER_QUERY_OAUTH=true` alongside the existing
`TRACER_OIDC_ISSUER`, `TRACER_OIDC_CLIENT_ID`, `TRACER_OIDC_CLIENT_SECRET`,
and `TRACER_OIDC_CALLBACK` settings. The client ID and secret in these settings
belong to the tracer's browser login. The tracer derives its public HTTPS
origin from the callback URL.

With a callback of `https://tracer.example.com/auth/callback`, the OAuth
resource for project `default` is:

```text
https://tracer.example.com/api/v1/projects/default/mcp
```

Use the project slug and the exact URL without a trailing slash. Serve the
MCP endpoint and its `/.well-known/oauth-protected-resource/api/v1/projects/default/mcp`
metadata path through the same HTTPS reverse proxy. For remote connectors,
ChatGPT and Claude need to reach the tracer and Pocket ID discovery and
token endpoints.

## Configure Pocket ID and connect a client

1. In **Administration → APIs**, create an API for the exact resource URL
   above. Add the permission key `traces:read`. Repeat for each project you
   want to expose. Each project's API has its own grants.
2. For individually controlled grants, register an OAuth client for each
   connector in Pocket ID. Use the connector's exact redirect URI, enable
   PKCE, and grant only **User-delegated access** to the intended API and
   permission. Set **Allowed User Groups** on the client to control who can
   sign in. Choose the pre-registered client option in ChatGPT or **Use your
   own OAuth client** in Claude.
3. To use Client ID Metadata Documents (CIMD), allow the connector's exact
   metadata URL in Pocket ID's **Application Configuration → OIDC**. Then
   grant `traces:read` under the API's **Metadata document clients** access
   settings. That grant applies to every metadata client allowed by this
   Pocket ID instance. Use individually registered clients if you need
   different API grants for different connectors.
4. In ChatGPT on the web, add a custom MCP server with **OAuth** and the project MCP URL.
   Use the client metadata URL and redirect URI shown in ChatGPT's connection
   settings. In Claude Desktop, add a custom connector with the same MCP URL
   and choose **Use Claude's published identity** for CIMD. The Pocket ID
   demo uses `https://claude.ai/oauth/mcp-oauth-client-metadata` for Claude Desktop.

[OpenAI Docs on MCP OAuth](https://developers.openai.com/plugins/build/auth) ·
[Claude connector setup](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp) ·
[Pocket ID API grants](https://pocket-id.org/docs/guides/apis) ·
[Pocket ID MCP demo](https://github.com/pocket-id/mcp-oauth-demo)

## What the token authorizes

MCP, `POST /api/v1/projects/{project}/query`, and
`POST /api/v1/projects/{project}/query/export` accept the same access token
in the `Authorization: Bearer` header. REST clients should request the
project's MCP URL as the OAuth `resource` during authorization and token
exchange. Static project read credentials and browser administrator sessions
are also accepted.

The tracer verifies the JWT signature with the issuer's published keys,
checks the access-token type, exact issuer, project audience, expiration,
and subject, then requires `traces:read`. A token can also list the Pocket ID
issuer as an audience for userinfo. The grant includes captured output
when requested. It gives no permission to ingest, change
settings, or access another project. Pocket ID controls which users and
clients get that grant. Use short token lifetimes if removing a grant needs
to take effect quickly, since already-issued JWTs are verified locally
until expiration.

A missing or invalid token returns HTTP 401 and a `WWW-Authenticate`
challenge with the discovery URL. A valid token for the project that lacks
`traces:read` returns HTTP 403 with `insufficient_scope`.
