# The sandbox pod

One pod, two containers, one address.

| container | holds | can reach |
|---|---|---|
| `worker` | the proxy token | the proxy (metrics, repositories, DNS, cloud — and its own platform calls, forwarded) |
| `shell` | nothing | the proxy's IP, which returns 401 to it |

The two share `/work`, where repositories are unpacked, and the pod's
loopback, over which the worker forwards each `bash` call to the shell
(`internal/shell`). They do not share a process namespace, a user id, or
a credential.

The `NetworkPolicy` allows egress to exactly one `/32` and one port: the
proxy, as Docker's host gateway on a laptop or a Service on a real
cluster. No DNS (the pod's resolver is set to a loopback nobody serves),
no cluster API, no metadata service, no internet. The worker checks all
of that at start (`sandbox.VerifyConfinement`) and refuses to run if any
check fails — a policy nobody enforces is a comment, so kind runs Calico
here instead of its default CNI.

The platform credential is not in the pod. The worker's client points at
`<proxy>/anthropic`, and the proxy (`internal/proxy/agentapi.go`) forwards
nine allowlisted paths — claim, heartbeat, release, the session's event
stream — swapping a placeholder bearer for the environment key. Nothing
that creates a session, lists others, manages agents or downloads
anything passes.

## Bring it up

```sh
hack/sandbox/up.sh        # cluster + Calico, images, pod, policy; idempotent
hack/sandbox/verify.sh    # proves the confinement from inside the shell
```

The proxy runs on the laptop (`change-agent proxy --config proxy.yaml`),
with `token: keychain:paved-proxy-token` — `up.sh` generates the token
into the Keychain and copies it into the pod's Secret. Then a review from
the laptop is served by the pod:

```sh
change-agent review --config proxy.yaml --pr smallStepGiantLeap/hello#3 --worker=false --budget 1
```

## What verify.sh checks

Everything the E5 review was able to do from the laptop, from inside the
shell container: no container runtime, not root, read-only system, no
token in the environment, no view of the worker's processes, no route to
the internet, DNS, the cluster API or the metadata service; a route to
the proxy's port that refuses the shell. And the worker's log line
`confinement verified`.
