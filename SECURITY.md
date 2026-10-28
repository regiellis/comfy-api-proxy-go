# Security

comfy-api-proxy can expose a GPU machine to a network, so its defaults are restrictive: it
binds loopback only, refuses a non-loopback bind without a bearer token, blocks cross-site
browser requests, and places model files only behind header, path, and no-overwrite checks.
[Remote access](docs/remote-access.md#security-defaults) lists each default and what it
protects.

## Supported versions

Fixes land on the latest release. Before 1.0 there are no backports to older minor versions.

## Reporting a vulnerability

Report it privately through GitHub: open the repository's **Security** tab and choose
**Report a vulnerability**. Please do not open a public issue for a security problem.

Include the version (`comfy-api-proxy --version`), how the proxy was configured (with any
token removed), and the requests that show the problem.

## Scope

In scope: this binary and the `sdk` package, including the bind gate, bearer auth, the origin
guard and CORS, upload handling and model placement, output asset id signing, and the state
directory.

Out of scope: ComfyUI itself and its custom nodes, and a deployment that turns the defaults off
on purpose (`allow_insecure_bind` on an untrusted network, for example). Problems the Python
proxy shares should also go to [Comfy-Org/comfy-api-proxy](https://github.com/Comfy-Org/comfy-api-proxy).
