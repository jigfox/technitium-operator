# technitium-operator

A Kubernetes operator that manages [Technitium DNS Server](https://technitium.com/dns/)
settings from custom resources, so they can live in Git next to the rest of
your cluster configuration.

- `TechnitiumServer`, `TechnitiumAllowedDomain` and `TechnitiumBlockedDomain`
  are namespaced resources in the API group `technitium.jigfox.github.io/v1alpha1`.
- A `TechnitiumServer` is one Technitium node: where it is, how to log in, and
  the settings the operator manages on it (block list URLs, the configuration of
  installed DNS apps, optionally pruning of unmanaged domains).
- Existing allowed and blocked entries are **adopted**, not recreated.
- Only what you declare is managed. A setting you do not write is never
  touched, an app config is merged key by key, and entries without a resource
  stay (unless you opt in to [pruning](#pruning)).
- A `--dry-run` mode shows what the operator would change without sending
  anything to Technitium.

It talks to Technitium over its HTTP API only (web service, default port 5380).
Tested against Technitium DNS Server 15.6.2.

## Install

```sh
kubectl apply -f https://github.com/jigfox/technitium-operator/releases/download/vX.Y.Z/install.yaml
```

This creates the namespace `technitium-operator-system`, the CRDs, RBAC and the
Deployment. The image is `ghcr.io/jigfox/technitium-operator` (`linux/amd64`
and `linux/arm64`). The operator needs its own resources, events, leader
election leases and `get` on Secrets (to read the credentials, see below).

### Credentials

Each `TechnitiumServer` names a Secret **in its own namespace**:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: technitium-credentials
  namespace: dns
stringData:
  username: admin
  password: change-me
```

`spec.credentialsSecretRef` selects what is used:

| Field | Meaning |
| --- | --- |
| `name` | The Secret. Required. |
| `username` | The account name as a literal. If set, `usernameKey` is not used (handy when the Secret only holds a password). |
| `usernameKey` | Key of the account name, default `username`. |
| `passwordKey` | Key of the password, default `password`. |
| `tokenKey` | Key of an API token. If set, the token is used and username and password are ignored. Create one in Technitium under *My Profile* (*Create API Token*): it does not expire and needs no session. |

The account needs the permissions of what you manage: *Allowed* and *Blocked*
(view, modify, delete) for domains, *Settings* for block lists, *Apps* for app
configs. The built-in `admin` has all of them; a dedicated user in a group with
exactly those is better.

The Secret is read on every reconcile, so a rotated password applies without a
restart. The operator keeps one login session per server and ends it when the
credentials or the URL change, the `TechnitiumServer` is deleted or the
operator stops.

## Examples

```yaml
apiVersion: technitium.jigfox.github.io/v1alpha1
kind: TechnitiumServer
metadata:
  name: primary
  namespace: dns
spec:
  url: http://technitium-0.dns.svc:5380
  credentialsSecretRef:
    name: technitium-credentials
    username: admin
    passwordKey: ADMIN_PASSWORD
  pruneUnmanaged:            # default: both false
    allowedDomains: false
    blockedDomains: false
  settings:                  # not set: settings are not touched
    blockListUrls:
      - https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
  apps:
    - name: Query Logs (Sqlite)
      config:                # only these keys are set, the rest of the config stays
        enableLogging: true
        maxLogDays: 14
---
apiVersion: technitium.jigfox.github.io/v1alpha1
kind: TechnitiumAllowedDomain
metadata:
  name: example-allowed
  namespace: dns
spec:
  serverRef:
    name: primary
  domain: example.com
  reason: needed by an app that a block list breaks
---
apiVersion: technitium.jigfox.github.io/v1alpha1
kind: TechnitiumBlockedDomain
metadata:
  name: tracker
  namespace: dns
spec:
  serverRef:
    name: primary
  domain: tracker.example.org
  deletionPolicy: Orphan     # keep the entry when this resource is deleted
```

```console
$ kubectl -n dns get technitiumservers,technitiumalloweddomains,technitiumblockeddomains
NAME                                                URL                                 VERSION   READY   AGE
technitiumserver.technitium.jigfox.github.io/primary   http://technitium-0.dns.svc:5380   15.6.2    True    1m

NAME                                                                DOMAIN        SERVER    REASON                                READY   ADOPTED   AGE
technitiumalloweddomain.technitium.jigfox.github.io/example-allowed   example.com   primary   needed by an app that a block ...   True    false     1m
...
```

### TechnitiumServer

| Field | Meaning |
| --- | --- |
| `spec.url` | Base URL of the web service, `http://` or `https://`. |
| `spec.credentialsSecretRef` | See [Credentials](#credentials). |
| `spec.pruneUnmanaged` | `allowedDomains` / `blockedDomains`, both default `false`. See [Pruning](#pruning). |
| `spec.settings.blockListUrls` | The block list URLs. They **replace** the live list; `[]` clears it; not set leaves it alone. The operator writes only when the list differs, because Technitium re-downloads the lists on every write. URLs must not contain a comma (Technitium's separator). |
| `spec.apps[]` | `name` of an installed DNS app and `config`, the keys to set in its configuration. See [Apps](#apps). |
| `status` | `version`, `observedGeneration`, conditions. |

`settings` is a struct so that more settings can be added without breaking
existing resources.

### TechnitiumAllowedDomain, TechnitiumBlockedDomain

Both kinds have the same spec; they manage Technitium's *Allowed* and
*Blocked* zones respectively. Technitium treats an entry as the domain and all
its subdomains.

| Field | Meaning |
| --- | --- |
| `spec.serverRef.name` | The `TechnitiumServer` in the same namespace. Immutable. |
| `spec.domain` | The domain. Case and a trailing dot do not matter: the operator lowercases it and strips the dot (Technitium rejects a trailing dot). Immutable; to change it, create a new resource and delete the old one. |
| `spec.reason` | Optional free text for `kubectl get`. Not sent to Technitium. |
| `spec.deletionPolicy` | `Delete` (default) or `Orphan`. |
| `status` | `domain` (the normalised entry), `adopted`, `observedGeneration`, conditions. |

## Semantics

**Adoption.** On first reconcile the operator lists the entries of the server.
If the domain is there it is taken over and `status.adopted` is set to `true`;
if not, it is added. A domain resource never changes any other entry.

**Deletion.** Domain resources have a finalizer. With `deletionPolicy: Delete`
the entry is removed from Technitium (also if it was adopted), with `Orphan` it
stays and only the finalizer is removed. A resource that never reached
Technitium (no `status.domain`) deletes nothing. If the `TechnitiumServer` is
already gone, the finalizer is removed and the entry stays (with a Warning
Event), because waiting would block the deletion forever: delete the domain
resources before their server.

**Clusters.** In a Technitium cluster the allowed and blocked lists written on
the primary reach the secondaries on their own (within about 10 seconds), so
point the domain resources at the primary's `TechnitiumServer` only. App
configurations are **not** synced: list `apps` on the `TechnitiumServer` of
every node.

**Drift.** Every resource is re-checked every `--sync-period` (default 5m), so
changes made in the Technitium UI are corrected. Resources that wait for
something (a missing server, wrong credentials) are retried after a minute, and
a `TechnitiumServer` that appears releases the resources waiting for it.

**Conditions.** `Ready` (`True` when Technitium matches the spec) with the
reasons `Reconciled`, `ServerNotFound`, `CredentialsError` (no Secret, missing
key, or Technitium refused the login), `TechnitiumError` (the message is the
error), `AppError`, `Invalid`, `DryRunPending`. `status.observedGeneration`
follows `metadata.generation`.

### Apps

`spec.apps[].config` is merged over the live configuration of the installed
app: the top-level keys you give are set (a nested object or list is replaced
as a whole), every other key keeps its value. The operator writes only if a
given key differs from the live value (`30` and `30.0` are equal), so a second
pass is a no-op and a change made in the UI is undone on the next sync. The
operator does not install apps. An app that is not installed makes `Ready`
`False` with reason `AppError` and the app's name in the message; the other
apps and settings are still applied.

### Pruning

With `pruneUnmanaged.allowedDomains` or `blockedDomains` the operator deletes
every entry of that list that no resource of this server covers (compared as
normalised, exact names). It is off by default, because it also removes what
someone added in the UI.

- A resource that is being deleted still counts until it is gone, so an
  `Orphan` entry survives the deletion of its resource, but is pruned on the
  next sync after that: with pruning on, nothing outlives its resource.
- The managed set is read from the operator's cache. The operator never prunes
  before the cache is synced (`Ready` is `False` with "not pruning" instead),
  and not if listing the resources fails.
- `blockedDomains` prunes only the manually added blocked zones, not the
  domains that come from block lists.
- Resources that point at another `TechnitiumServer` do not cover entries of
  this one.

Try it with `--dry-run` first: every deletion shows up as an Event.

## Dry run

Start the manager with `--dry-run` to compute every change, log it and emit an
Event (`DryRun: would add "example.com" to the allowed list`, `would delete
unmanaged ...`) without sending a single mutation to Technitium. Logins and
reads still happen. Status is still updated: a `DryRun` condition is `True`,
and a resource that would change gets `Ready=False`, reason `DryRunPending`; one
that needs no change is `Ready=True`, so a rollout over an existing Technitium
can prove that adoption is a no-op. Finalizers are still managed; deleting a
resource in dry-run mode removes nothing from Technitium.

```sh
kubectl -n technitium-operator-system patch deployment technitium-operator-controller-manager \
  --type json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--dry-run"}]'
kubectl get events -A --field-selector reason=DryRun
```

## Flags

| | |
| --- | --- |
| `--dry-run` | See above. |
| `--sync-period` | Drift check interval, default `5m`. |
| `--leader-elect`, `--metrics-bind-address`, `--health-probe-bind-address`, ... | Standard kubebuilder flags. |

## Security notes

- Whoever can create these resources, or read the credentials Secrets, can
  change what Technitium blocks and allows, and can write app configuration
  (some apps, such as the SQL log apps, hold connection strings). Restrict RBAC
  on these kinds and on the Secrets, and consider a network policy that only
  lets the operator reach Technitium's web port.
- The operator reads Secrets with `get` through a ClusterRole, so it can read
  any Secret it is given the name of by a `TechnitiumServer`, in that server's
  namespace. It does not list or watch Secrets. If you want to narrow this, replace
  the ClusterRole rule by a Role with `resourceNames` in the namespaces that
  hold the credentials.
- A `TechnitiumServer` makes the operator send the credentials of its Secret to
  `spec.url`. Whoever can create a `TechnitiumServer` in a namespace can
  therefore send any Secret of that namespace of which they know the name to a
  URL of their choice. Do not let untrusted users create `TechnitiumServer`s.
- Use `https://` for `spec.url` if the traffic leaves the node. Certificates are
  verified; there is no switch to skip that.
- Sessions: with a password, the operator logs in once per server and keeps the
  session token in memory; it logs out when it stops. A crash leaves the session
  to expire on the server (30 minutes by default). An API token is never logged
  out.
- The container runs as non-root with a read-only root filesystem and no
  capabilities. Requests: 10m CPU / 64Mi, memory limit 128Mi.

## Development

Everything runs through [mise](https://mise.jdx.dev): `mise.toml` pins Go,
golangci-lint, kubebuilder, controller-gen, kustomize and setup-envtest, and
defines the tasks (there is no Makefile). `mise install` once, then:

```sh
mise run test               # generate, manifests, fmt, vet, then unit tests + envtest
mise run test-integration   # against a real Technitium container, needs Docker
mise run lint               # golangci-lint with the logcheck plugin; lint:fix to autofix
mise run manifests          # CRDs and RBAC, after changing api/ or RBAC markers
mise run generate           # DeepCopy methods
mise run build              # bin/manager
IMG=ghcr.io/jigfox/technitium-operator:dev mise run build-installer   # dist/install.yaml
mise tasks                  # all tasks (also docker:build, docker:buildx, install, deploy, ...)
```

`kubebuilder` scaffolding commands (`create api`, ...) still work, but any
Makefile target they would add has to be mirrored as a mise task by hand.

The integration tests default to `technitium/dns-server:15.6.2`; set
`TECHNITIUM_IMAGE` to try another tag. The tests of the app config install
*Query Logs (Sqlite)* from Technitium's app store, which needs internet access
from the container; without it they are skipped. With Colima, point
`DOCKER_HOST` at its socket and set
`TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`.

Layout: `api/v1alpha1` (types), `internal/technitium` (client behind an
interface, `fake` for tests), `internal/controller` (reconcilers, the client
provider that reads the credentials), `config/` (kustomize).

Releases are automatic, see below.

## Development and releases

Changes reach `main` only through pull requests, merged with **rebase merge**
(every commit of the PR lands on `main` as it is). A PR needs green checks
(Lint, Unit and envtest, Integration (real Technitium), Conventional commits)
and an up-to-date branch.

Every commit must be a [Conventional Commit](https://www.conventionalcommits.org)
(`type(scope): description`, `!` or a `BREAKING CHANGE:` footer for breaking
changes) with one of the types `feat`, `fix`, `perf`, `docs`, `test`,
`chore`, `ci`, `build`, `refactor`, `style` or `revert`; the check fails the
PR otherwise.

On every push to `main` the Release workflow looks at the commits since the
last `v*` tag and, if they warrant it, tags, builds and publishes a release
(multi-arch image `ghcr.io/jigfox/technitium-operator:<version>` and `:latest`,
`install.yaml` and generated release notes on the GitHub release). If nothing
is releasable the workflow ends without a release.

| Commit type | Release |
| --- | --- |
| `feat` | minor |
| `fix`, `perf` | patch |
| breaking (`!` / `BREAKING CHANGE:`) | minor while the version is 0.x, major from 1.0 on |
| `docs`, `test`, `chore`, `ci`, `build`, `refactor`, `style` | none |

Renovate uses `fix(deps)` for Go modules and the Dockerfile (they release) and
`chore(deps)` for GitHub Actions and mise tools.

A PR from this repository (not a fork) with the label `prerelease` gets a
pre-release `v<next version>-pr.<PR number>.<run number>` on every push to the
branch: image tag and GitHub pre-release, never `:latest`. When the PR is
closed its pre-releases and tags are deleted (the images stay).

`mise install` also installs git hooks with lefthook (`lefthook.yml`): every
commit message is checked on commit and every commit since `origin/main` on
push, with the same rules as CI.

The same logic runs locally (git-cliff, configured in `cliff.toml`):

```sh
mise run release:next             # next version, empty if nothing is releasable (--or-patch: at least a patch)
mise run release:notes -- 0.3.0   # release notes of the unreleased commits
mise run commits:check            # all commits in origin/main..HEAD conventional? (optional base branch argument)
mise run commits:check-msg <file> # one commit message (lefthook commit-msg hook)
mise run lint:workflows           # actionlint and zizmor
```

## License

MIT, see [LICENSE](LICENSE).
