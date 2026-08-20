# API-based VM restart

## What was built

New package `internal/kubevirt` (`kubevirt.go` + `kubevirt_test.go`):

```go
func Restart(ctx context.Context, kv *kube.KubeVirtClient, namespace, name string) error
```

Builds a `rest.Interface` off `kv.RESTConfig` via `rest.RESTClientFor`, scoped to
`kubevirtv1.SubresourceGroupVersions[0]` (`subresources.kubevirt.io/v1`) with
`APIPath = "/apis"`. `NegotiatedSerializer` is `client-go/kubernetes/scheme.Codecs`
(already a transitive dependency, no new one added) — it doesn't know KubeVirt
types, but it doesn't need to: the empty `kubevirtv1.RestartOptions{}` body is
marshalled by hand with `encoding/json` and sent as raw `[]byte` (bypassing
`runtime.Object` encoding entirely), and the scheme's job is only to let
`apierrors.IsNotFound`/`IsConflict` correctly decode the apiserver's
`metav1.Status` error responses. Verified this holds even when the server
returns a bare status code with no body (client-go's `NewGenericServerResponse`
fallback still infers the right `Reason` from the HTTP status).

Sends `PUT .../namespaces/{ns}/virtualmachines/{name}/restart` with an empty
`RestartOptions` body — no `GracePeriodSeconds` set, so KubeVirt falls back to
the VMI's own `terminationGracePeriodSeconds`, which is the right default
absent a reason to override it.

## Error mapping
- 404 → `"no VirtualMachine %s/%s"` (via `apierrors.IsNotFound`)
- 409 → `"... cannot be restarted right now — it is most likely not running"` (via `apierrors.IsConflict`)
- anything else → wrapped generically, original error preserved with `%w`

## Rewired call sites
- `cmd/changemachineclass.go` `maybeRestart` — dropped the `VirtctlTarget()` pre-check entirely (no longer applicable); calls `kubevirt.Restart`.
- `cmd/rollout.go` — the `Restarter` closure now calls `kubevirt.Restart`; removed `ensureVirtctl`/`virtctl.Path` gate from `prepareRestart` (rollout no longer touches virtctl at all).
- `cmd/vm.go` `newVMActionCmd` — explicit `if a.verb == "restart"` branch calling `kubevirt.Restart`, with a comment explaining why the other verbs stay on virtctl (VMI-subresource semantics for pause/unpause/soft-reboot; streaming for console/vnc).

## Preflight relaxed
`internal/roll/phases.go` `Preflight` no longer calls `m.KV.VirtctlTarget()` — restart needs nothing from it now. Doc comment rewritten to state the real precondition (guest node must exist); guest-node check kept intact.

## Left on virtctl (deliberately)
`start`, `stop`, `reset`, `pause`, `unpause`, `soft-reboot`, `console`, `vnc` — per the task's own reasoning (VMI-subresource semantics differ; console/vnc are streaming). Comments added at each retained site.

## Verification
```
go build ./...        → clean
go vet ./...           → clean
go test ./... -count=1 → ok, all 12 packages
gofmt -l               → empty
go mod tidy            → no diff to go.mod/go.sum
make lint               → 0 issues
```
New tests in `internal/kubevirt/kubevirt_test.go` (httptest + `rest.Config{Host: srv.URL}`): request-shape (PUT + exact path), success, 404, 409. Updated `cmd/changemachineclass_test.go` (discovered-cluster restart now succeeds instead of being refused; API-failure test replaces virtctl-missing test) and `internal/roll/phases_test.go` (`TestPreflightPassesWithoutLocalKubeconfig` replaces `TestPreflightCatchesUnrestartableCluster`).

## Not touched
`internal/roll/targets.go` — left for the concurrent clusterId agent, per instructions.

## Concerns
None outstanding. `go.mod`/`go.sum` unchanged (confirmed via diff).
