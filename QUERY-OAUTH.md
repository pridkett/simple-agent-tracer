# OAuth for model-call queries

Pocket ID can issue access tokens directly to Codex, ChatGPT, and Claude Desktop for
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

## Choose an MCP resource

Use the shared endpoint for one connection that can query multiple projects.
With a browser callback of `https://tracer.example.com/auth/callback`, its
resource is `https://tracer.example.com/api/v1/mcp`. Forward that path and
`/.well-known/oauth-protected-resource/api/v1/mcp` through the HTTPS proxy.

On the tracer, set the projects each OAuth user can read, then restart it.
Use the user's exact access-token subject (`sub`) from the configured issuer.

```sh
export TRACER_QUERY_OAUTH_PROJECTS='{"your-pocket-id-user-sub":["default","jimbo"]}'
```

A subject with no entry or an empty array sees no projects. An explicit
`["*"]` for a subject includes every current and future project. A static
read token sees the project assigned to that credential. Local administrators
and signed-in browser administrators can query every project.

For a connection dedicated to one project, use the existing resource
`https://tracer.example.com/api/v1/projects/default/mcp`, substituting the
desired slug. Its metadata path is
`/.well-known/oauth-protected-resource/api/v1/projects/default/mcp`. In this
mode Pocket ID controls access separately for each project. An existing
project-specific grant continues to work on that resource.

## Configure Pocket ID and connect a client

1. In **Administration → APIs**, create an API with the exact resource URL
   you chose and add the permission key `traces:read`. The shared endpoint
   needs one API. Dedicated project endpoints each need their own API.
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
4. In ChatGPT on the web, add a custom MCP server with **OAuth** and the chosen MCP URL.
   Use the client metadata URL and redirect URI shown in ChatGPT's connection
   settings. In Claude Desktop, add a custom connector with the same MCP URL
   and choose **Use Claude's published identity** for CIMD. The Pocket ID
   demo uses `https://claude.ai/oauth/mcp-oauth-client-metadata` for Claude Desktop.

[OpenAI Docs on MCP OAuth](https://developers.openai.com/plugins/build/auth) ·
[Claude connector setup](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp) ·
[Pocket ID API grants](https://pocket-id.org/docs/guides/apis) ·
[Pocket ID MCP demo](https://github.com/pocket-id/mcp-oauth-demo)

## Connect Codex to the shared endpoint

Reuse the registered public client's ID and callback in `~/.codex/config.toml`.

```toml
[mcp_servers.simple-agent-tracer]
url = "https://tracer.example.com/api/v1/mcp"

[mcp_servers.simple-agent-tracer.oauth]
client_id = "<registered-public-client-id>"
callback_url = "http://127.0.0.1/callback"
```

Run `codex mcp login simple-agent-tracer` after changing the resource URL.
Call `list_projects`, then pass a returned slug in the `project` argument of
`query_model_calls`. For the loopback callback, register the path shown by Codex
and allow the port selected at login. [Codex MCP configuration](https://developers.openai.com/codex/mcp/)

If Pocket ID reports that the resource or scope is invalid, compare the
`resource` in the tracer's public metadata with the API in Pocket ID. Check
that the public client has **User-delegated access** to `traces:read` for that
exact API. If login succeeds but `list_projects` is empty, check the subject
mapping on the tracer and that the listed project slugs exist.

## What the token authorizes

Project-specific MCP, `POST /api/v1/projects/{project}/query`, and
`POST /api/v1/projects/{project}/query/export` accept the same access token
in the `Authorization: Bearer` header. REST clients should request the
project's MCP URL as the OAuth `resource` during authorization and token
exchange. Static project read credentials and browser administrator sessions
are also accepted.

The tracer verifies the JWT signature with the issuer's published keys,
checks the access-token type, exact issuer, requested resource audience, expiration,
and subject, then requires `traces:read`. A token can also list the Pocket ID
issuer as an audience for userinfo. The grant includes captured output
when requested. Shared-resource tokens authorize MCP queries only for the
subject's locally granted projects. Project-resource tokens authorize reads
only for their project. Pocket ID controls which users and clients can obtain
each grant. Use short token lifetimes if removing a grant needs
to take effect quickly, since already-issued JWTs are verified locally
until expiration.

A missing or invalid token returns HTTP 401 and a `WWW-Authenticate`
challenge with the discovery URL. A valid token for the requested resource that lacks
`traces:read` returns HTTP 403 with `insufficient_scope`.
